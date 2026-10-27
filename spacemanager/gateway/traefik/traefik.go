package traefik

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lalternativefabrique/sklp/ingress"
	"github.com/lalternativefabrique/sklp/space"
	"github.com/lalternativefabrique/sklp/spacemanager"
)

// serviceName is the runc id stem of the gateway singleton container in the
// standalone (host) space. Stable so EnsureUp/Down/Status all address the same
// container across CLI invocations.
const serviceName = "sklp-gateway-traefik"

// dynMount / acmeMount / cfgMount are the IN-CONTAINER paths the host gateway dir
// is exposed at. The whole gateway dir is bind-mounted once at cfgMount, so the
// static config, the dyn dir, and acme.json are all reachable under it.
const (
	cfgMount  = "/etc/traefik"
	dynMount  = cfgMount + "/dynamic"
	acmeMount = cfgMount + "/acme.json"
	logMount  = cfgMount + "/traefik.log"
	cfgFile   = cfgMount + "/traefik.yaml"
)

func init() {
	// Register out-of-package so spacemanager.OpenGateway("traefik") works once the CLI
	// blank-imports this package — without package deploy importing any traefik
	// code. fake stays seeded inside deploy; this is the only vendor driver edge.
	spacemanager.RegisterGatewayDriver("traefik", New)
}

// traefikGateway is the traefik GatewayDriver: a single host-level traefik owning
// :9080/:9443 + acme.json, reading the shared file-provider dyn dir into which sklp
// writes one router file per (space, service). It is the multi-space sibling of
// the per-space traefik proxy raised to the host front door.
//
// Route/Unroute + the static-config generation are fully exercised by unit tests
// (pure render + FS ops). The real container launch (EnsureUp/Down/Status) is
// integration-only (no privilege/network in CI), so it stays uncovered by unit
// tests; the spec-level guard that the launch omits the netns lives in the
// runtime package.
//
// HOST NETWORKING: EnsureUp launches the singleton with HostNetwork=true, so it
// shares the host netns and binds the unprivileged :9080/:9443 directly on the
// host — no CAP_NET_BIND_SERVICE needed under the user namespace. The container
// keeps its user namespace + read-only rootfs isolation. On a public host, an
// nftables redirect :80→:9080 / :443→:9443 fronts it (deployment wiring).
//
// ENVIRONMENTAL (not a code gap): ACME/Let's Encrypt issuance still requires
// public DNS for the routed hosts to point at this host's public IP and inbound
// :80/:443 (redirected to :9080/:9443) to be reachable. That is deployment wiring, outside
// the runtime's control.
type traefikGateway struct {
	mu sync.Mutex

	dir       string // HOST-level gateway config dir
	dynDir    string // file-provider dir (one router file per space-service)
	acmePath  string // traefik-owned cert store
	cfgPath   string // static config file
	statePath string // records the singleton is "up" (survives across CLI calls)

	space *space.Space // the standalone (host) space running the singleton
}

// New builds the traefik driver against the host-level gateway dir
// (spacemanager.GatewayDir, $XDG_CONFIG_HOME-honoured for tests). Side-effect free: it
// touches the FS only on EnsureUp/Route, so constructing one is cheap. It is the
// spacemanager.GatewayConstructor registered under "traefik".
func New() (spacemanager.GatewayDriver, error) {
	dir := spacemanager.GatewayDir()
	return &traefikGateway{
		dir:       dir,
		dynDir:    filepath.Join(dir, "dynamic"),
		acmePath:  filepath.Join(dir, "acme.json"),
		cfgPath:   filepath.Join(dir, "traefik.yaml"),
		statePath: filepath.Join(dir, "up.state"),
	}, nil
}

// EnsureUp makes the singleton ready, idempotently: it writes the static config +
// the dyn dir + an empty acme.json, then launches the traefik container (unless
// already alive) and records the "up" state. A second call with a live container
// is a no-op. An image that cannot be pulled offline returns a clear error and
// never hangs (the runtime image prep fails fast).
func (g *traefikGateway) EnsureUp() error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if err := g.writeStaticAssets(); err != nil {
		return err
	}

	// Already up with a live container: nothing to do (idempotent).
	if g.containerAlive() {
		return g.markUp()
	}

	sp, err := space.NewSpace("")
	if err != nil {
		return fmt.Errorf("traefik gateway: open host space: %w", err)
	}
	g.space = sp

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// Bind the whole host gateway dir at cfgMount so the static config, dyn dir,
	// and acme.json are all reachable inside the container, and point traefik at
	// the static config file. Image prep (pull/unpack) fails fast offline.
	vols := []string{g.dir + ":" + cfgMount}
	// HostNetwork shares the host netns but NOT the host rootfs: without a resolver
	// the container's /etc/resolv.conf points at nothing, so ACME can't look up
	// acme-v02.api.letsencrypt.org. Mount the host resolver read-only (we share its
	// netns, so its upstream DNS is reachable). Skip if absent (no-op for offline).
	if _, err := os.Stat("/etc/resolv.conf"); err == nil {
		vols = append(vols, "/etc/resolv.conf:/etc/resolv.conf:ro")
	}
	acme, _ := resolvedACME(g.cfgPath)
	if _, err := sp.Start(ctx, space.DeployService{
		Name:    serviceName,
		Image:   imageTag,
		Command: []string{"--configFile=" + cfgFile},
		Volumes: vols,
		// traefik's built-in lego dnsChallenge provider (e.g. "ovh") reads its DNS
		// API credentials from these same env vars — no custom wiring needed.
		Env: dnsCredentialEnv(acme.DNSCredentials),
		// Share the host netns so traefik binds the host's :9080/:9443 directly.
		HostNetwork: true,
	}); err != nil {
		return fmt.Errorf("traefik gateway: launch %s: %w", imageTag, err)
	}
	return g.markUp()
}

// resolvedACME reads the gateway.acme config exported by spacemanager.Parse
// into SKLP_SPACE_ACME (dns_provider + resolved DNS credentials), and falls back
// to the copy persisted beside traefik.yaml when that variable is absent.
//
// The variable is only ever set by a deploy, which is the process that reads
// deploy.yaml. Every other start — `sklp gateway up`, the supervision watchdog,
// a reboot — used to see nothing here and rewrite the static config with
// httpChallenge, silently dropping a dnsChallenge a deploy had configured. That
// is how a working wildcard setup was downgraded into an issuance loop that
// burned Let's Encrypt rate limits on unrelated hosts.
//
// The persisted copy carries no secrets: it names the credential env vars, and
// their values are read back from the environment here.
func resolvedACME(cfgPath string) (ingress.ACMEConfig, bool) {
	cfg, ok, err := ingress.UnmarshalACMEEnv(os.Getenv(ingress.EnvACME))
	if err == nil && ok {
		return cfg, true
	}
	return loadACMEConfig(cfgPath)
}

// dnsCredentialEnv turns a resolved credential map (key -> value) into
// "KEY=value" entries for the container process env. traefik's lego providers
// read these directly (e.g. OVH_APPLICATION_KEY), so no translation is needed.
func dnsCredentialEnv(creds map[string]string) []string {
	if len(creds) == 0 {
		return nil
	}
	env := make([]string, 0, len(creds))
	for k, v := range creds {
		env = append(env, k+"="+v)
	}
	return env
}

// writeStaticAssets renders + writes the static config (atomic), creates the dyn
// dir, and ensures an acme.json exists (0600 — traefik refuses a world-readable
// cert store). Idempotent: re-runs overwrite the config and leave acme.json.
func (g *traefikGateway) writeStaticAssets() error {
	if err := os.MkdirAll(g.dynDir, 0o755); err != nil {
		return fmt.Errorf("traefik gateway: dyn dir: %w", err)
	}
	acme, ok := resolvedACME(g.cfgPath)
	if !ok {
		if err := acmeConfigError(g.cfgPath); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}
	saveACMEConfig(g.cfgPath, acme)
	cfg := renderStaticConfig(dynMount, acmeMount, logMount, os.Getenv("SKLP_GATEWAY_ACME_EMAIL"), acme.DNSProvider)
	if err := atomicWrite(g.cfgPath, []byte(cfg), 0o644); err != nil {
		return fmt.Errorf("traefik gateway: static config: %w", err)
	}
	// traefik requires the acme storage file to exist and be 0600.
	if _, err := os.Stat(g.acmePath); os.IsNotExist(err) {
		if werr := os.WriteFile(g.acmePath, []byte("{}\n"), 0o600); werr != nil {
			return fmt.Errorf("traefik gateway: acme store: %w", werr)
		}
	} else if err != nil {
		return fmt.Errorf("traefik gateway: stat acme store: %w", err)
	}
	return nil
}

// markUp records the "up" state file so Status (and a later CLI invocation that
// reconstructs the driver) knows the singleton was started.
func (g *traefikGateway) markUp() error {
	if err := os.WriteFile(g.statePath, []byte("up\n"), 0o644); err != nil {
		return fmt.Errorf("traefik gateway: mark up: %w", err)
	}
	return nil
}

// containerAlive reports whether the singleton container is running. It needs a
// space handle; a freshly reconstructed driver (no g.space) re-opens the host
// space to query. Best-effort: any error reads as not-alive.
func (g *traefikGateway) containerAlive() bool {
	sp := g.space
	if sp == nil {
		var err error
		if sp, err = space.NewSpace(""); err != nil {
			return false
		}
		g.space = sp
	}
	return sp.Running(serviceName)
}

// Down stops the singleton container (best-effort, idempotent) and clears the
// "up" state. A missing container / state file is not an error.
func (g *traefikGateway) Down() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	sp := g.space
	if sp == nil {
		if s, err := space.NewSpace(""); err == nil {
			sp = s
		}
	}
	if sp != nil {
		sp.Stop(serviceName)
	}
	if err := os.Remove(g.statePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("traefik gateway: mark down: %w", err)
	}
	return nil
}

// Status reports REAL liveness: the container running (not merely the state
// file). The state file is the fallback signal when a container query is
// unavailable, so `gateway status` is still informative on a host where the
// runtime cannot introspect.
func (g *traefikGateway) Status() (spacemanager.GatewayStatus, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	running := g.containerAlive()
	if !running {
		// Fall back to the persisted intent so a driver reconstructed in a separate
		// process (e.g. `sklp gateway status`) still reports "up" between a launch
		// and the container query path landing fully.
		if _, err := os.Stat(g.statePath); err == nil {
			running = true
		} else if !os.IsNotExist(err) {
			return spacemanager.GatewayStatus{}, fmt.Errorf("traefik gateway: stat state: %w", err)
		}
	}
	return spacemanager.GatewayStatus{Driver: "traefik", Running: running, Listening: portWeb + "/" + portWebsecure}, nil
}

// Route declares/replaces the route for d.Host by writing a namespaced router
// file <space>-<service>.yaml into the dyn dir (traefik's file provider
// hot-reloads it). It emits a real Host(`host`) router + (when d.TLS) a
// tls.certResolver, with one server url per resolved backend. An empty backend
// set still writes the router (empty servers), so a re-Route on backend arrival
// just rewrites the file.
func (g *traefikGateway) Route(space, service string, routes []spacemanager.Domain) error {
	if space == "" || service == "" {
		return fmt.Errorf("traefik gateway: route requires a space and service")
	}
	// An empty route set is an unroute: drop the vendor file so a service that
	// lost all its routes stops being fronted.
	if len(routes) == 0 {
		return g.Unroute(space, service)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := os.MkdirAll(g.dynDir, 0o755); err != nil {
		return fmt.Errorf("traefik gateway: dyn dir: %w", err)
	}
	rs := make([]route, 0, len(routes))
	for _, d := range routes {
		rs = append(rs, route{
			Host: d.Host, Path: d.Path, Backends: d.Backends, TLS: d.TLS,
			RedirectApex: d.RedirectApex, SecurityHeaders: d.SecurityHeaders, Compress: d.Compress,
			Rate: d.Rate, Burst: d.Burst, Timeout: d.Timeout, MaxBody: d.MaxBody,
			BackendHost: d.BackendHost, Passthrough: d.Passthrough,
		})
	}
	body := renderRoutes(space, service, rs)
	if err := atomicWrite(g.routeFilePath(space, service), []byte(body), 0o644); err != nil {
		return fmt.Errorf("traefik gateway: write route %s/%s: %w", space, service, err)
	}
	return nil
}

// Unroute removes the vendor file owning (space, service)'s routes. Keyed by the
// deterministic file name, so a host shared with a sibling service is untouched.
// Idempotent: an absent file (or missing dyn dir) is not an error.
func (g *traefikGateway) Unroute(space, service string) error {
	if space == "" || service == "" {
		return fmt.Errorf("traefik gateway: unroute requires a space and service")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := os.Remove(g.routeFilePath(space, service)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("traefik gateway: remove route %s/%s: %w", space, service, err)
	}
	return nil
}

// routeFilePath namespaces the dyn-dir file per (space, service):
// <space>-<service>.yaml, so two spaces routing the same service never collide.
func (g *traefikGateway) routeFilePath(space, service string) string {
	return filepath.Join(g.dynDir, routerName(space, service)+".yaml")
}

// PruneRoutes garbage-collects orphan route files: it removes every *.yaml in
// the dyn dir whose (space, service) is NOT in keep. A supervisor SIGKILLed
// before its teardown Unroute runs leaves a stale router file pointing at a dead
// backend; the controller calls this once at gateway-up with the keys every live
// space declares, so the on-disk route set reconciles back to the desired set.
//
// It works off the EXPECTED filenames (routerName of each kept key), not by
// parsing names back out of the files — routerName is lossy (sanitizeFile), so
// the on-disk name is the only reliable key. Any leftover atomic-write .tmp is
// also swept. Idempotent; an empty keep set prunes every route file.
func (g *traefikGateway) PruneRoutes(keep map[spacemanager.RouteKey]bool) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	wanted := make(map[string]struct{}, len(keep))
	for k := range keep {
		wanted[routerName(k.Space, k.Service)+".yaml"] = struct{}{}
	}

	entries, err := os.ReadDir(g.dynDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no dyn dir yet => nothing routed => nothing to prune
		}
		return fmt.Errorf("traefik gateway: read dyn dir: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Sweep stale atomic-write temp files unconditionally; otherwise only
		// touch the route files we own (*.yaml) and keep the wanted ones.
		isTmp := strings.HasSuffix(name, ".tmp")
		if !isTmp {
			if !strings.HasSuffix(name, ".yaml") {
				continue
			}
			if _, ok := wanted[name]; ok {
				continue
			}
		}
		if rerr := os.Remove(filepath.Join(g.dynDir, name)); rerr != nil && !os.IsNotExist(rerr) {
			return fmt.Errorf("traefik gateway: prune route %s: %w", name, rerr)
		}
	}
	return nil
}

// containsHost reports whether body declares the given Host(`...`) rule.
func containsHost(body, needle string) bool {
	for i := 0; i+len(needle) <= len(body); i++ {
		if body[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// atomicWrite writes data to a sibling .tmp file then renames it over path, so a
// reader (traefik's file provider) never observes a half-written config.
func atomicWrite(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
