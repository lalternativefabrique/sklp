// Package sklet is the node-side half of the deploy split: everything that only
// makes sense ON the machine being deployed to. It owns the supervisor loop, the
// __supervise daemon, the local launch modes (systemd --user / setsid) and the
// space teardown. sklp keeps the client half — it talks to a node over the
// control API; sklet IS the node.
package sklet

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/lalternativefabrique/sklp/sklet/logship"
	"github.com/lalternativefabrique/sklp/space"
	"github.com/lalternativefabrique/sklp/spacemanager"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
	"github.com/lalternativefabrique/sklp/spacemanager/controller"
	"golang.org/x/sys/unix"
)

// SuperviseOptions tunes a supervisor loop run.
type SuperviseOptions struct {
	// Out/ErrOut are the destinations for live feedback and warnings (the
	// terminal for foreground, journald-captured stdout/stderr under systemd).
	Out, ErrOut *os.File
	// OnAllReady, if set, is called once every service is Ready.
	OnAllReady func()
	// Proxied says a reverse proxy must be wired for this space.
	Proxied bool
	// GatewayDriver, when non-empty, names the host-gateway driver to bring up
	// for this space (set only when at least one resolved service is domained).
	// Empty = no gateway is opened (no front door, no traefik launched).
	GatewayDriver string
	// Listen, when non-empty (e.g. ":9443"), additionally exposes the control API
	// on a TCP+TLS network listener guarded by a Bearer token — the remote pilot
	// path. The local Unix socket is always served regardless.
	Listen string
}

// GatewayDriverFor returns the gateway driver name to wire for a space, or ""
// when the space declares no gateway route (no front door is opened, so no
// gateway is launched). It keeps the "open a gateway only if needed" policy in
// one place, shared by the foreground and __supervise entry points.
func GatewayDriverFor(meta spacemanager.SpaceMeta, driver string) string {
	if !spacemanager.HasGatewayRoute(meta) {
		return ""
	}
	return driver
}

// SuperviseLoop is the supervisor body shared by the foreground deploy and the
// systemd-detached __supervise daemon. It takes the per-space lock, builds the
// runc launcher (+ proxy), opens the control socket, and runs the agent until
// ctx is cancelled (Ctrl-C foreground, SIGTERM under systemd). It does NOT read
// the cwd: services+meta are passed in (resolved from deploy.yaml in the
// foreground path, from the persisted manifest in __supervise).
func SuperviseLoop(ctx context.Context, services []spacemanager.Service, meta spacemanager.SpaceMeta, sopts SuperviseOptions) error {
	// Single-supervisor guard: take the per-space flock BEFORE any launcher /
	// bridge / socket is created. Contention fails here cleanly — no bridge, no
	// double-bound port, no crash-loop. Held for the supervisor's whole life.
	releaseLock, err := control.AcquireSpaceLock(meta.Name)
	if err != nil {
		return err
	}
	defer releaseLock()

	// Serve the node's whole space inventory alongside this space's own routes,
	// so an operator view can enumerate the machine without knowing a space name.
	installInventory(sopts.ErrOut)

	// Derive a cancellable child so POST /v1/down can stop this supervisor from
	// the inside on the SAME path as SIGTERM: cancelling ctx drives the agent
	// teardown + socket/pidfile cleanup. The handler calls this after acking 202.
	ctx, shutdown := context.WithCancel(ctx)
	defer shutdown()

	// The space lifecycle (open gateway → build the agent → serve the control
	// API → run the reconcile loop → teardown) is owned by the SpaceController.
	// SuperviseLoop keeps only the cmd-level concerns: the single-supervisor
	// flock (above), arg/config parsing (caller), and the opt-in network control
	// listener's cert/token/print plumbing (passed in as a closure so the
	// controller stays free of that CLI-specific surface).
	sc := controller.New()
	spaceOpts := controller.SpaceOptions{
		Meta:          meta,
		Services:      services,
		GatewayDriver: sopts.GatewayDriver,
		Proxied:       sopts.Proxied,
		OnAllReady:    sopts.OnAllReady,
		Listen:        sopts.Listen,
		Out:           sopts.Out,
		ErrOut:        sopts.ErrOut,
		Shutdown:      shutdown,
	}
	// NetListener itself gates whether runSpace calls it (see controller.go),
	// so it must stay nil when Listen is unset — otherwise a foreground/local
	// `sklp deploy` with no --listen would still try to open a "" address.
	if sopts.Listen != "" {
		spaceOpts.NetListener = func(ctx context.Context, space string, sup control.Supervisor, spaceShutdown func()) {
			StartNetListener(ctx, space, sup, sopts.Listen, spaceShutdown, sopts.Out, sopts.ErrOut)
		}
	}
	return sc.Run(ctx, spaceOpts)
}

// StartNetListener brings up the opt-in network control API for a space: it
// ensures the persisted self-signed cert + token, prints where/how to connect
// (address, token file, fingerprint), then serves ServeNet in the background.
// Best-effort: a cert/token error or a port bind failure is a WARNING — the local
// socket keeps serving so the deploy is never blocked on the remote path.
func StartNetListener(ctx context.Context, space string, agent control.Supervisor, addr string, shutdown func(), out, errOut *os.File) {
	cert, fingerprint, err := control.EnsureServerCert(space)
	if err != nil {
		fmt.Fprintf(errOut, "sklet: network API disabled (cert): %v\n", err)
		return
	}
	token, err := control.EnsureToken(space)
	if err != nil {
		fmt.Fprintf(errOut, "sklet: network API disabled (token): %v\n", err)
		return
	}

	if _, shipStop, serr := logship.Start(ctx, space, control.NodeName(), logshipSupervisor{agent}); serr != nil {
		fmt.Fprintf(errOut, "sklet: log shipping disabled: %v\n", serr)
	} else {
		go func() { <-ctx.Done(); shipStop() }()
	}
	server := fmt.Sprintf("https://%s%s", listenHost(), addr)
	fmt.Fprintf(out, "sklet: control API listening on https://0.0.0.0%s\n", addr)
	tokenPath := control.TokenPath(space)
	// The token is named by its 0600 file, never printed: this output is the
	// systemd journal, readable by every process of the user and shipped by any
	// log collector on the node — which would undo the file's permissions.
	fmt.Fprintf(out, "  token file:  %s\n", tokenPath)
	fmt.Fprintf(out, "  fingerprint: %s\n", fingerprint)
	fmt.Fprintf(out, "\nRemote control ready. On your workstation run:\n")
	fmt.Fprintf(out, "  sklp deploy login --server %s --token \"$(ssh %s cat %s)\" --ca-fingerprint %s\n", server, listenHost(), tokenPath, fingerprint)
	fmt.Fprintf(out, "(substitute the host with this VM's public address if it differs; then `sklp deploy ls` needs no flags)\n")
	go func() {
		if serr := control.ServeNet(ctx, space, agent, addr, cert, token, shutdown); serr != nil {
			fmt.Fprintf(errOut, "sklet: network control API stopped (local socket still serving): %v\n", serr)
		}
	}()
}

// logshipSupervisor adapts control.Supervisor to logship.Supervisor, so
// logship takes no dependency on spacemanager's concrete LogSource type.
type logshipSupervisor struct{ control.Supervisor }

func (a logshipSupervisor) ServiceLogSources(name string) []logship.LogSource {
	srcs := a.Supervisor.ServiceLogSources(name)
	out := make([]logship.LogSource, len(srcs))
	for i, s := range srcs {
		out[i] = logship.LogSource{Label: s.Label, Path: s.Path}
	}
	return out
}

// listenHost returns a best-effort host for the printed login block: the
// outbound (LAN) IP if one can be discovered, else the resolved hostname, else
// "this-host". It never errors — the user can substitute the public address.
func listenHost() string {
	if conn, err := net.Dial("udp", "8.8.8.8:80"); err == nil {
		defer conn.Close()
		if la, ok := conn.LocalAddr().(*net.UDPAddr); ok && la.IP != nil {
			return la.IP.String()
		}
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "this-host"
}

// RunSupervise is the internal `sklet __supervise --space X` daemon, started by
// systemd OR by the daemonless setsid detach. It reads the PERSISTED manifest
// (NOT the cwd — at boot the original repo may be gone), records its pid, GCs
// this space's stale runc state, then runs the supervisor loop until SIGTERM
// (systemd stop, or `delete` via the pidfile). Logs go to stdout/stderr:
// journald captures them under systemd, the per-space log file when detached.
func RunSupervise(args []string) {
	fs := flag.NewFlagSet("__supervise", flag.ExitOnError)
	spaceName := fs.String("space", "", "space to supervise (required)")
	_ = fs.Bool("foreground", false, "(accepted, ignored) __supervise always runs in the foreground of its own process")
	fs.Parse(args)

	if *spaceName == "" {
		fmt.Fprintln(os.Stderr, "sklet __supervise: --space is required")
		os.Exit(2)
	}

	services, meta, err := control.LoadManifest(*spaceName)
	if err != nil {
		// A missing manifest means the space was torn down (its artifacts were
		// removed) while systemd relaunched __supervise. Do NOT loop on
		// Restart=on-failure: disable our own unit best-effort and exit 0 so the
		// space stays down. A genuinely corrupt manifest also lands here; the
		// message points at it.
		if os.IsNotExist(errors.Unwrap(err)) || os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "sklet __supervise: no manifest for space %q; it was torn down — exiting\n", *spaceName)
			if control.SystemdAvailable() {
				_ = control.DisableUnit(*spaceName)
			}
			return
		}
		fmt.Fprintln(os.Stderr, "sklet __supervise:", err)
		os.Exit(1)
	}

	// Record our pid so `sklp deploy delete` can SIGTERM this daemon when there is
	// no systemd unit to stop it (daemonless detach). Removed at teardown so a
	// later down does not act on a stale pid. Best-effort under systemd (the unit
	// is the source of truth there); the pidfile is harmless redundancy.
	if perr := control.WriteSupervisorPid(*spaceName, os.Getpid()); perr != nil {
		fmt.Fprintln(os.Stderr, "sklet __supervise: warning: could not write pidfile:", perr)
	}
	defer control.RemoveSupervisorPid(*spaceName)

	// A daemon must NOT die on SIGHUP: systemd --user starts __supervise from the
	// launcher's session, and when that terminal hangs up the inherited SIGHUP's
	// default disposition would TERMINATE us (the observed signal=HUP kill). Ignore
	// it explicitly so the space survives the launching shell closing. Teardown is
	// driven only by SIGINT/SIGTERM (systemd stop, delete) below.
	signal.Ignore(syscall.SIGHUP)

	// GC this space's stale runc state: at reboot the prior containers are dead
	// but their runc state/bundles may linger. Stop them best-effort (do NOT
	// adopt orphans) so the agent starts from a clean slate under the same ids.
	GCSpaceContainers(*spaceName, services)

	// Re-open the network control API if this space was deployed with --listen
	// (the addr is persisted next to the manifest). Missing => socket-only.
	listen, lerr := control.LoadListenAddr(*spaceName)
	if lerr != nil {
		fmt.Fprintln(os.Stderr, "sklet __supervise: warning: could not read persisted --listen address:", lerr)
	}

	// Reconstruct the gateway driver persisted at deploy time; missing => default.
	gatewayDriver, gderr := control.LoadGatewayDriver(*spaceName)
	if gderr != nil {
		fmt.Fprintln(os.Stderr, "sklet __supervise: warning: could not read persisted gateway driver:", gderr)
	}
	if gatewayDriver == "" {
		gatewayDriver = spacemanager.DefaultGatewayDriver
	}

	// SIGTERM (systemd stop) cancels ctx -> agent teardown + socket close.
	ctx, stop := signal.NotifyContext(context.Background(), unix.SIGINT, unix.SIGTERM)
	defer stop()

	if err := SuperviseLoop(ctx, services, meta, SuperviseOptions{
		Out:     os.Stdout,
		ErrOut:  os.Stderr,
		Proxied: spacemanager.HasProxiedService(services),
		// No DeployConfig at boot (manifest-only): use the gateway driver persisted
		// at deploy time (default when none was recorded), so a reboot reconstructs
		// the SAME front door.
		GatewayDriver: GatewayDriverFor(meta, gatewayDriver),
		Listen:        listen,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "sklet __supervise:", err)
		if errors.Is(err, spacemanager.ErrConfig) {
			// Restarting cannot clear this, so exit on the code the unit refuses to
			// restart on: the service lands in `failed` with this message, instead
			// of relaunching every RestartSec forever against a human-only fix.
			os.Exit(control.ExitConfigError)
		}
		os.Exit(1)
	}
}

// GCSpaceContainers stops any runc-state container belonging to this space's
// resolved services before the supervisor starts. It matches the space's
// instance ids against the live deploy runc state and Stops the intersection —
// scoped to THIS space (no orphan adoption). Best-effort: a listing failure or
// a missing container is ignored (a fresh host simply has nothing to GC).
func GCSpaceContainers(spaceName string, services []spacemanager.Service) {
	// List + launcher are BOTH scoped to this space's space, so the GC only
	// ever sees and stops this space's containers — never another space's.
	infos, err := space.ListDeployContainers(spaceName)
	if err != nil || len(infos) == 0 {
		return
	}
	// runc ids this space's instances would carry (DeployInstanceID folds in
	// the space name + sanitizes), so we compare on the resolved instance ids.
	want := make(map[string]struct{})
	for _, svc := range services {
		for _, inst := range spacemanager.InstanceIDs(svc) {
			want[space.DeployInstanceID(spaceName, inst)] = struct{}{}
		}
	}
	launcher, _, err := spacemanager.NewRuncLauncher(spaceName)
	if err != nil {
		return
	}
	for _, info := range infos {
		if _, ours := want[info.ID]; !ours {
			continue // another space's container: do NOT adopt or touch
		}
		// Stop takes the launcher instance id (without the runc prefix); strip it.
		launcher.Stop(strings.TrimPrefix(info.ID, space.DeployInstanceID(spaceName, "")))
	}
}

// SupervisionBanner is the one-line cue printed the moment every service is
// Ready in the foreground path: the agent now stays alive to supervise until it
// is told to stop. Ephemeral spaces already announced their TTL at launch.
func SupervisionBanner(meta spacemanager.SpaceMeta, services []spacemanager.Service) string {
	stop := "press Ctrl-C to tear down"
	if meta.Ephemeral && meta.TTL > 0 {
		stop = fmt.Sprintf("auto-teardown in %v (or Ctrl-C)", meta.TTL)
	}
	return fmt.Sprintf("\n▶ supervising %s — %s\n", pluralServices(len(services)), stop)
}

// pluralServices formats a service count: "1 service" / "2 services".
func pluralServices(n int) string {
	if n == 1 {
		return "1 service"
	}
	return fmt.Sprintf("%d services", n)
}

// TeardownSpace stops every instance of every resolved service by id. It is
// the manual drop of a space (and the manual eject of an ephemeral preview):
// a fresh process resolving the same space yields the same instance ids the
// agent used, so Stop(<svc>-<i>) reaches the running containers. Best-effort
// and idempotent — Stop tolerates an already-gone instance.
func TeardownSpace(spaceName string, services []spacemanager.Service) error {
	// Launcher scoped to THIS space's space, so Stop reaches the containers in
	// that space's runc state root (per-space since D2) — a -down of `production`
	// never touches another space's containers, and DOES find its own. The
	// space network of the prior `deploy` process is already gone with it.
	launcher, _, err := spacemanager.NewRuncLauncher(spaceName)
	if err != nil {
		return fmt.Errorf("init runc launcher: %w", err)
	}
	for _, svc := range services {
		for _, id := range spacemanager.InstanceIDs(svc) {
			launcher.Stop(id)
		}
	}
	return nil
}

// DeployForeground is the interactive supervisor: it streams every state
// transition to the terminal and reconciles until Ctrl-C / TTL. Used with
// --foreground or when systemd --user is unavailable.
func DeployForeground(cfg *spacemanager.DeployConfig, services []spacemanager.Service, meta spacemanager.SpaceMeta, listen string) error {
	ctx, stop := signal.NotifyContext(context.Background(), unix.SIGINT, unix.SIGTERM)
	defer stop()

	// TTL enforcement (ephemeral spaces): arm a timer that cancels the run.
	if meta.Ephemeral && meta.TTL > 0 {
		fmt.Printf("space %q is ephemeral; auto-teardown in %v\n", meta.Name, meta.TTL)
		t := time.AfterFunc(meta.TTL, func() {
			fmt.Printf("\nspace %q TTL (%v) elapsed; tearing down\n", meta.Name, meta.TTL)
			stop()
		})
		defer t.Stop()
	}
	if meta.URL != "" {
		fmt.Printf("  url: %s\n", meta.URL)
	}

	return SuperviseLoop(ctx, services, meta, SuperviseOptions{
		Out:           os.Stdout,
		ErrOut:        os.Stderr,
		Proxied:       cfg.ProxyServiceName() != "",
		GatewayDriver: GatewayDriverFor(meta, cfg.GatewayDriverName()),
		Listen:        listen,
		OnAllReady:    func() { fmt.Fprint(os.Stdout, SupervisionBanner(meta, services)) },
	})
}
