package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lalternativefabrique/sklp/space"
	"github.com/lalternativefabrique/sklp/spacemanager"
)

// Supervisor is the view the control server needs of a running spacemanager.Agent.
// *spacemanager.Agent satisfies it; tests pass a fake so the server is exercised
// without launching real containers. Read methods back ps/logs; the mutation
// methods back POST /v1/apply (idempotent reconcile of the live set).
type Supervisor interface {
	AllReady() bool
	Snapshot() []spacemanager.ServiceSnapshot
	HasService(name string) bool
	HostBackend(name string) (string, error)
	// ServiceLogSources returns the labeled log files of a service's live
	// instances; empty when the service has none / no log paths are exposed.
	ServiceLogSources(name string) []spacemanager.LogSource
	// RecentLogLines returns the buffered tail of a service (the no-follow dump
	// fallback when no on-disk log file is reachable).
	RecentLogLines(name string) []string

	// DesiredServiceNames lists the currently-desired service names — the live
	// set apply diffs the incoming manifest against.
	DesiredServiceNames() []string
	// SetRegistryAuths updates the private-registry pull credentials the launcher
	// uses for subsequent Starts. A remote apply calls this with the manifest's
	// resolved auths (password shipped over the control API) so newly-added
	// services can pull private images the detached agent's env can't authorise.
	// A no-op when the launcher has no registry support (the exec test launcher).
	SetRegistryAuths(auths map[string]space.RegistryAuth)
	// AddService registers a new desired service; the reconcile loop starts it.
	AddService(svc spacemanager.Service)
	// RemoveService drops a service from the desired set and tears its instances
	// down (stop + proxy/ingress/DNS removal).
	RemoveService(name string)
	// Reconfigure updates an existing service's desired spec (rolling update on a
	// runnable change, in-place scale/route otherwise). Unknown services no-op.
	Reconfigure(svc spacemanager.Service)
	// ServiceDiffers reports whether the desired spec of an existing service
	// differs from the live one (runnable change OR scale/route change). An
	// unknown service reports false (it is an add, not a reconfigure).
	ServiceDiffers(svc spacemanager.Service) bool
	// DesiredService returns the spec a known service is converging to.
	DesiredService(name string) (spacemanager.Service, bool)
	// RejectedSpec reports whether svc is the spec whose rollout was just rolled
	// back, which Reconfigure refuses to replay, with the rollback's reason.
	RejectedSpec(svc spacemanager.Service) (string, bool)
	// SetDeclaredImage records the image Git declares for a running service
	// without a rollout.
	SetDeclaredImage(name, image string)

	// SetRoutes replaces the space's gateway route set, then re-routes every
	// affected service so a re-apply that adds, changes, or removes routes
	// converges the live front door. Returns whether the set actually changed;
	// an identical set is a no-op that returns false.
	SetRoutes(routes []spacemanager.ResolvedRoute) bool

	// ResolveExecTarget returns the runc coordinates of a live instance of a
	// service so the server can `runc exec` into it (sklp deploy exec). ok is
	// false when the service is unknown / has no running instance / cannot exec.
	ResolveExecTarget(name string) (spacemanager.ExecTarget, bool)
	// RestartService bounces every live instance of a service (stop + reconcile
	// relaunch). Returns false when the service is unknown.
	RestartService(name string) bool
	// TriggerJobErr re-runs a oneshot job that reached a terminal state, back
	// from the spec the node holds. The error distinguishes an unknown service,
	// a service that is not a job, and a run already in flight.
	TriggerJobErr(name string) error

	// OpenControllerIngress opens a 127.0.0.1:hostPort -> containerPort listener
	// for a service, independent of the manifest's own ingress: declaration.
	// The Network Controller (spacemanager/netctrl) calls this so it can reach
	// a service's real backend from the host netns even when the service
	// declares only port: and no ingress: mapping of its own.
	OpenControllerIngress(name string, hostPort, containerPort int)

	// GatewayMode returns this space's authoritative front-door mode (singleton |
	// per-space), as resolved by THIS node at supervisor construction. handleApply
	// uses it to overwrite an incoming apply's spec.Meta.GatewayMode: the mode is
	// a node-owned property, never something a remote deploy.yaml may set.
	GatewayMode() string
}

// Serve runs the control HTTP API for a space on ln until ctx is cancelled.
// It closes ln on ctx done so an in-flight Serve returns. Routes:
//
//	GET  /v1/health             -> HealthResponse
//	GET  /v1/state              -> StateResponse (supervisor's whole-space view)
//	GET  /v1/logs/<svc>         -> text dump of the service's buffered log tail
//	GET  /v1/logs/<svc>?follow=1 -> chunked text stream until the client cuts
//	POST /v1/apply              -> ApplyResult (idempotent reconcile)
//	POST /v1/down               -> DownResponse (202, then triggers shutdown)
//
// shutdown is the supervisor's "stop yourself" hook (typically the supervisor
// ctx's CancelFunc): POST /v1/down acks 202 then calls it AFTER flushing the
// ack, so the supervisor tears down on the SAME path as SIGTERM (teardown +
// socket/pidfile cleanup) while the client returns promptly. May be nil (e.g.
// foreground): /v1/down then reports it is unsupported. The follower is bound to
// the request context: a disconnected `logs -f` cancels r.Context() and the
// per-line tail goroutines stop — no leaked FollowLog.
func Serve(ctx context.Context, space string, sup Supervisor, ln net.Listener, shutdown func()) error {
	return ServeWithPurge(ctx, space, sup, ln, shutdown, nil)
}

// ServeWithPurge is Serve with the deletion variant of the teardown, so
// POST /v1/down can tell "stop this space" from "delete it".
func ServeWithPurge(ctx context.Context, space string, sup Supervisor, ln net.Listener, shutdown, purgeShutdown func()) error {
	mux := newMuxWithPurge(space, sup, shutdown, purgeShutdown)
	srv := &http.Server{Handler: mux}
	// Close the listener when ctx is done; http.Serve then returns ErrServerClosed.
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	err := srv.Serve(ln)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// newMux builds the control API route table shared by the LOCAL Unix-socket
// listener (Serve) and the NETWORK TLS listener (ServeNet). The handlers are
// identical on both transports — the network listener only adds a token-auth
// middleware (tokenAuth) in front of this mux; the local socket trusts the
// caller. Factoring the mux here is what guarantees ps/logs/apply/exec/down
// behave the same whether reached over the socket or the network.
func newMux(space string, sup Supervisor, shutdown func()) *http.ServeMux {
	return newMuxWithPurge(space, sup, shutdown, nil)
}

func newMuxWithPurge(space string, sup Supervisor, shutdown, purgeShutdown func()) *http.ServeMux {
	return newMuxWithDelete(space, sup, shutdown, purgeShutdown, nil)
}

func newMuxWithDelete(space string, sup Supervisor, shutdown, purgeShutdown, deleteShutdown func()) *http.ServeMux {
	since := time.Now()
	mux := http.NewServeMux()
	// applyMu serialises concurrent /v1/apply calls so two clients never
	// interleave Add/Remove/Reconfigure against the same desired set. The agent's
	// own mutex protects each individual mutation; this guards the diff+mutate
	// sequence as a unit.
	var applyMu sync.Mutex

	mux.HandleFunc(RouteApply, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "apply: POST required", http.StatusMethodNotAllowed)
			return
		}
		applyMu.Lock()
		defer applyMu.Unlock()
		res, err := handleApply(space, sup, r.Body)
		if err != nil {
			http.Error(w, "apply: "+err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, res)
	})

	mux.HandleFunc(RouteImage, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "image: POST required", http.StatusMethodNotAllowed)
			return
		}
		// Same mutex as apply: an image roll IS an apply, and the two must never
		// interleave their diff+mutate against one desired set.
		applyMu.Lock()
		defer applyMu.Unlock()
		res, err := handleImage(space, sup, r.Body)
		if err != nil {
			http.Error(w, "image: "+err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, res)
	})

	mux.HandleFunc(RouteGatewayRoutes, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "gateway routes: PUT required", http.StatusMethodNotAllowed)
			return
		}
		// Serialised against /v1/apply: both converge the route set, and
		// interleaving them would let a stale apply undo a fresh route push.
		applyMu.Lock()
		defer applyMu.Unlock()
		var req GatewayRoutesRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "gateway routes: "+err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, GatewayRoutesResponse{
			Changed: sup.SetRoutes(req.Routes),
			Routes:  len(req.Routes),
		})
	})

	mux.HandleFunc(RouteDown, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "down: POST required", http.StatusMethodNotAllowed)
			return
		}
		if shutdown == nil {
			http.Error(w, "down: this supervisor cannot self-stop (foreground)", http.StatusNotImplemented)
			return
		}
		// An absent/!purge body STOPS the space: containers go, the manifest
		// and volumes stay, so it can be started again. purge:true is the
		// deletion `sklp deploy delete` performs. Both used to purge, which
		// made the UI's Stop button erase a space it promised only to stop.
		var req DownRequest
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&req)
		}
		stop := shutdown
		if req.Purge && purgeShutdown != nil {
			stop = purgeShutdown
		}
		// Volumes only reads on a purge: a stop that dropped the data would
		// betray exactly what the Stop button promises to keep.
		if req.Purge && req.Volumes && deleteShutdown != nil {
			stop = deleteShutdown
		}
		// Ack BEFORE the teardown so the client does not wait on the (possibly
		// slow) container stop: write 202 + flush, then trigger the shutdown.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(DownResponse{Stopping: true})
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Cancel the supervisor ctx in the background: identical to SIGTERM. Done
		// after the response so this handler returns and the connection closes.
		go stop()
	})

	mux.HandleFunc(RouteHealth, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, HealthResponse{
			Space: space,
			PID:   os.Getpid(),
			Since: since.Format(time.RFC3339),
		})
	})

	mux.HandleFunc(RouteState, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, buildState(space, sup))
	})

	mux.HandleFunc(RouteManifest, func(w http.ResponseWriter, r *http.Request) {
		serveManifest(w, r, space)
	})

	mux.HandleFunc(RouteRestores, serveRestores)
	mux.HandleFunc(RoutePublicNIC, servePublicNIC)

	mux.HandleFunc(RouteDataStores, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "datastores: GET required", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, DataStoresResponse{Space: space, Stores: spacemanager.DataStores(desiredServices(sup))})
	})

	mux.HandleFunc(RouteSpaces, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "spaces: GET required", http.StatusMethodNotAllowed)
			return
		}
		inv := currentInventory()
		if inv == nil {
			http.Error(w, "spaces: this node serves no inventory", http.StatusNotImplemented)
			return
		}
		spaces, err := inv.SpacesJSON(r.Context())
		if err != nil {
			http.Error(w, "spaces: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, SpacesResponse{Node: NodeName(), Spaces: spaces})
	})

	mux.HandleFunc(RouteExec, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "exec: POST required", http.StatusMethodNotAllowed)
			return
		}
		handleExec(r.Context(), w, r, sup)
	})

	mux.HandleFunc(RouteDial, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "dial: POST required", http.StatusMethodNotAllowed)
			return
		}
		handleDial(r.Context(), w, r, space, sup)
	})

	mux.HandleFunc(RouteRestart, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "restart: POST required", http.StatusMethodNotAllowed)
			return
		}
		svc := strings.TrimPrefix(r.URL.Path, RouteRestart)
		if err := validServiceName(svc); err != nil {
			http.Error(w, "restart: "+err.Error(), http.StatusBadRequest)
			return
		}
		if !sup.RestartService(svc) {
			http.Error(w, "restart: unknown service "+svc, http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc(RouteRun, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "run: POST required", http.StatusMethodNotAllowed)
			return
		}
		job := strings.TrimPrefix(r.URL.Path, RouteRun)
		if err := validServiceName(job); err != nil {
			http.Error(w, "run: "+err.Error(), http.StatusBadRequest)
			return
		}
		switch err := sup.TriggerJobErr(job); {
		case err == nil:
			w.WriteHeader(http.StatusOK)
		case errors.Is(err, spacemanager.ErrJobUnknown):
			http.Error(w, "run: unknown service "+job, http.StatusNotFound)
		case errors.Is(err, spacemanager.ErrJobNotOneshot):
			http.Error(w, "run: "+job+" is not a job", http.StatusBadRequest)
		case errors.Is(err, spacemanager.ErrJobInFlight):
			http.Error(w, "run: "+job+" already has a run in flight", http.StatusConflict)
		default:
			http.Error(w, "run: "+err.Error(), http.StatusInternalServerError)
		}
	})

	mux.HandleFunc(RouteLogs, func(w http.ResponseWriter, r *http.Request) {
		svc := strings.TrimPrefix(r.URL.Path, RouteLogs)
		if svc == "" {
			http.Error(w, "logs: service name required", http.StatusBadRequest)
			return
		}
		if !sup.HasService(svc) {
			http.Error(w, "logs: unknown service "+svc, http.StatusNotFound)
			return
		}
		follow := r.URL.Query().Get("follow") != ""
		serveLogs(r.Context(), w, sup, svc, follow)
	})

	return mux
}

// nonZeroTime keeps a zero instant off the wire: encoding/json never omits a
// struct, so a zero time.Time would ship as "0001-01-01T00:00:00Z" and read
// as a real instant to every consumer.
func nonZeroTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// buildState maps the supervisor snapshot to the wire DTO.
func buildState(space string, sup Supervisor) StateResponse {
	snap := sup.Snapshot()
	resp := StateResponse{
		Space:    space,
		AllReady: sup.AllReady(),
		Services: make([]ServiceState, 0, len(snap)),
	}
	for _, s := range snap {
		ss := ServiceState{
			Name:           s.Name,
			State:          s.State.String(),
			Image:          s.Image,
			Oneshot:        s.Oneshot,
			RejectedReason: s.RejectedReason,
		}
		for _, inst := range s.Instances {
			ss.Instances = append(ss.Instances, InstanceState{
				ID:        inst.ID,
				State:     inst.State.String(),
				PID:       inst.PID,
				IP:        inst.IP,
				Port:      inst.Port,
				Restarts:  inst.Restarts,
				StartedAt: nonZeroTime(inst.StartedAt),
				RestartAt: nonZeroTime(inst.RestartAt),
				BackoffMS: inst.Backoff.Milliseconds(),
				JobFails:  inst.JobFails,
				Held:      inst.Held,
			})
		}
		for _, r := range s.Rollouts {
			ss.Rollouts = append(ss.Rollouts, RolloutState{
				ID:         r.ID,
				FromImage:  r.FromImage,
				ToImage:    r.ToImage,
				Result:     r.Result,
				Reason:     r.Reason,
				ExitCode:   r.ExitCode,
				LogTail:    r.LogTail,
				StartedAt:  r.StartedAt,
				FinishedAt: r.FinishedAt,
			})
		}
		for _, r := range s.Runs {
			ss.Runs = append(ss.Runs, JobRunState{
				ID:         r.ID,
				Instance:   r.Instance,
				Image:      r.Image,
				Trigger:    r.Trigger,
				ExitCode:   r.ExitCode,
				Failed:     r.Failed,
				LogTail:    r.LogTail,
				StartedAt:  r.StartedAt,
				FinishedAt: r.FinishedAt,
			})
		}
		resp.Services = append(resp.Services, ss)
	}
	return resp
}

// serveLogs streams a service's logs as "[label] line\n" text. Without follow
// it dumps the on-disk files (or the buffered tail when no file is reachable)
// then returns. With follow it tails every instance file until the request
// context is cancelled (client disconnect), stopping all followers — no leak.
func serveLogs(ctx context.Context, w http.ResponseWriter, sup Supervisor, svc string, follow bool) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	flusher, _ := w.(http.Flusher)
	emit := func(label, line string) {
		_, _ = w.Write([]byte("[" + label + "] " + line + "\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}

	sources := sup.ServiceLogSources(svc)

	if !follow {
		printed := dumpSources(sources, emit)
		if !printed {
			// No reachable on-disk file: fall back to the agent's buffered tail.
			for _, line := range sup.RecentLogLines(svc) {
				emit(svc, line)
			}
		}
		return
	}

	if len(sources) == 0 {
		// Nothing to follow (no log paths); emit the buffered tail and return so
		// the client does not block forever on an empty stream.
		for _, line := range sup.RecentLogLines(svc) {
			emit(svc, line)
		}
		return
	}

	// One follower per instance; all bound to ctx (request cancel = client gone).
	done := make(chan struct{}, len(sources))
	for _, s := range sources {
		s := s
		go func() {
			spacemanager.FollowLog(ctx, s.Path, func(line string) { emit(s.Label, line) })
			done <- struct{}{}
		}()
	}
	for range sources {
		<-done
	}
}

// dumpSources reads each source's current contents once. Returns whether any
// line was emitted (so the caller can fall back to the buffered tail).
func dumpSources(sources []spacemanager.LogSource, emit func(label, line string)) bool {
	printed := false
	for _, s := range sources {
		err := spacemanager.DumpLog(s.Path, func(line string) {
			printed = true
			emit(s.Label, line)
		})
		if err != nil && !os.IsNotExist(err) {
			emit(s.Label, "log read error: "+err.Error())
		}
	}
	return printed
}

// handleExec runs a command inside a live instance of a service. It validates
// the name and resolves a live instance BEFORE any takeover so an unresolvable
// service/instance returns a clean 4xx (never a half-hijacked connection). With
// tty=1 it HIJACKS the connection and proxies a raw bidi stream to a PTY-backed
// `runc exec -t`; otherwise it streams stdin/stdout/stderr and reports the exit
// code in a trailer header.
func handleExec(ctx context.Context, w http.ResponseWriter, r *http.Request, sup Supervisor) {
	svc := strings.TrimPrefix(r.URL.Path, RouteExec)
	if err := validServiceName(svc); err != nil {
		http.Error(w, "exec: "+err.Error(), http.StatusBadRequest)
		return
	}
	target, ok := sup.ResolveExecTarget(svc)
	if !ok {
		http.Error(w, "exec: no running instance for service "+svc, http.StatusNotFound)
		return
	}

	var req ExecRequest
	if r.Body != nil {
		// A missing/empty body is fine (defaults to sh); a malformed one is an error.
		dec := json.NewDecoder(io.LimitReader(r.Body, 1<<16))
		if err := dec.Decode(&req); err != nil && err != io.EOF {
			http.Error(w, "exec: bad request body: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	cmd := req.Cmd
	if len(cmd) == 0 {
		cmd = []string{"sh"}
	}

	if r.URL.Query().Get("tty") != "" {
		handleExecTTY(ctx, w, r, target, cmd)
		return
	}
	handleExecStreamed(ctx, w, target, cmd)
}

// handleExecStreamed runs a non-interactive exec: stdin is the request body,
// stdout/stderr stream back in the response body, and the exit code rides in a
// declared trailer so the client can read it after the stream.
func handleExecStreamed(ctx context.Context, w http.ResponseWriter, target spacemanager.ExecTarget, cmd []string) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Trailer", HeaderExecExitCode)
	w.WriteHeader(http.StatusOK)
	code, err := space.RuncExecStreams(ctx, target.RuncBin, target.RuncRoot, target.ContainerID, cmd, nil, w, w)
	if err != nil {
		// Headers are already sent; surface the failure in the trailer as -1.
		code = -1
	}
	w.Header().Set(HeaderExecExitCode, strconv.Itoa(code))
}

// handleExecTTY hijacks the connection and proxies a raw bidi stream to a
// PTY-backed `runc exec -t`. It writes a minimal 101 switching-protocols line on
// the raw conn (the takeover handshake the client reads), then runs the exec; the
// connection is closed when the exec exits. rows/cols seed the initial window
// (resize is not propagated — documented limit).
func handleExecTTY(ctx context.Context, w http.ResponseWriter, r *http.Request, target spacemanager.ExecTarget, cmd []string) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "exec: server does not support connection hijacking", http.StatusInternalServerError)
		return
	}
	rows := parseUint16(r.URL.Query().Get("rows"))
	cols := parseUint16(r.URL.Query().Get("cols"))

	conn, _, err := hj.Hijack()
	if err != nil {
		http.Error(w, "exec: hijack failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer conn.Close()

	// Acknowledge the takeover so the client stops parsing HTTP and switches to
	// the raw stream. After this line every byte is the PTY session.
	if _, err := conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nConnection: upgrade\r\nUpgrade: sklp-exec\r\n\r\n")); err != nil {
		return
	}

	// A start failure (e.g. the container cannot allocate a console) leaves the
	// session mute otherwise: the 101 was already sent, so we cannot fall back to
	// an HTTP error. Relay the cause as raw bytes on the hijacked stream — the
	// client copies them straight to the user's terminal — so the failure is
	// visible instead of an instant silent return.
	if _, err := space.RuncExecTTY(ctx, target.RuncBin, target.RuncRoot, target.ContainerID, cmd, conn, rows, cols); err != nil {
		_, _ = conn.Write([]byte("sklp exec: " + err.Error() + "\r\n"))
	}
	// The exec ended: closing conn (deferred) signals end-of-session to the client.
}

// parseUint16 parses a non-negative decimal into a uint16, returning 0 on any
// parse error or overflow (callers treat 0 as "use the default window size").
func parseUint16(s string) uint16 {
	n, err := strconv.ParseUint(s, 10, 16)
	if err != nil {
		return 0
	}
	return uint16(n)
}

// Listen opens the space's control socket: it creates the parent dir 0700,
// removes a stale/orphan socket that nobody answers, binds a unix listener, and
// chmods it 0600 (owner-only). Returns the listener and the socket path so the
// caller can defer its removal.
func Listen(space string) (net.Listener, string, error) {
	socket := SocketPath(space)
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		return nil, socket, err
	}
	// A leftover socket from a crashed supervisor would make bind fail with
	// EADDRINUSE. If nobody answers it, it is orphaned — remove it before bind.
	if _, err := os.Stat(socket); err == nil {
		if !socketAlive(socket) {
			_ = os.Remove(socket)
		}
	}
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return nil, socket, err
	}
	_ = os.Chmod(socket, 0o600)
	return ln, socket, nil
}

// socketAlive reports whether a supervisor is currently accepting on socket.
func socketAlive(socket string) bool {
	c, err := net.DialTimeout("unix", socket, 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func desiredServices(sup Supervisor) []spacemanager.Service {
	names := sup.DesiredServiceNames()
	out := make([]spacemanager.Service, 0, len(names))
	for _, name := range names {
		if svc, ok := sup.DesiredService(name); ok {
			out = append(out, svc)
		}
	}
	return out
}
