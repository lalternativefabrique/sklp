// Package control is the local control-plane API of `sklp deploy`: the
// supervisor (the foreground deploy process) serves a small HTTP API over a
// per-space Unix socket, and `sklp deploy ps` / `logs` are CLIENTS of that
// socket. This makes ps/logs report the supervisor's truth from ANY directory,
// instead of re-deriving state from the runc state / the cwd manifest (which
// disagree when run outside the deployed repo).
//
// protocol.go is the shared, build-tag-free surface (socket path + JSON DTOs +
// route constants): it depends only on net/http + JSON so the client and the
// non-embedded CLI compile WITHOUT the embedded_runtime tag. server.go (which
// reads a live *deploy.Agent) is gated on embedded_runtime.
package control

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lalternativefabrique/sklp/internal/xdg"
	"github.com/lalternativefabrique/sklp/spacemanager"
)

// API version + route constants. The socket speaks plain HTTP/1.1; versioning
// the path lets a future J2+ (apply/down) add routes without breaking clients.
const (
	APIVersion = "v1"

	RouteHealth = "/v1/health"
	RouteState  = "/v1/state"
	// RouteLogs is the prefix; the service name is the trailing path segment:
	// GET /v1/logs/<svc>?follow=1&tail=N
	RouteLogs = "/v1/logs/"
	// RouteApply reconciles a running supervisor toward a desired manifest
	// (idempotent, k8s-style): POST the resolved space spec, get back what
	// changed. A 2nd `sklp deploy` of a live space talks here instead of
	// starting a competing supervisor.
	RouteApply = "/v1/apply"
	// RouteDown asks the supervisor to tear its space down from the INSIDE: the
	// handler acks 202 then cancels the supervisor's context, so the daemonless
	// detach stops exactly like a SIGTERM (teardown + socket/pidfile cleanup) with
	// no external signal. POST only.
	RouteDown = "/v1/down"

	// RouteStart brings a STOPPED space back up from the manifest it kept when
	// it stopped: POST /v1/spaces/<space>/start -> StartResponse (200).
	// Unlike apply it carries no spec — it restarts what was declared, it does
	// not redefine it — and unlike the implicit materialize-on-apply it says so
	// explicitly, so a typo cannot conjure an empty space by accident.
	RouteStart = "/v1/start"
	// RouteExec runs a command inside a live instance of a service (docker/kubectl
	// exec). The service name is the trailing path segment; the command is the
	// JSON request body. With tty=1 the handler HIJACKS the connection and proxies
	// a raw bidi stream to a PTY-backed `runc exec -t`; without it, stdin/stdout/
	// stderr are streamed and the exit code returned in a trailer header. POST.
	//   POST /v1/exec/<svc>?tty=1&rows=R&cols=C   body: {"cmd":[...]}
	RouteExec = "/v1/exec/"
	// RouteRestart bounces every live instance of a service (stop + reconcile
	// relaunch) without a full re-apply. The service name is the trailing segment.
	//   POST /v1/restart/<svc>
	RouteRestart = "/v1/restart/"
	// RouteRun re-runs a oneshot job that already reached a terminal state
	// (completed, or held failed past the retry cap), reusing the spec the node
	// already holds. A restart bounces a live process; a job has none to bounce,
	// so re-running one is a distinct verb. The job name is the trailing segment.
	//   POST /v1/run/<job>
	RouteRun = "/v1/run/"
	// RouteImage rolls ONE service onto a new image, reusing the manifest the
	// node already persisted: the caller names a service and a tag, not a whole
	// spec. A registry watcher can therefore roll a deploy out without holding
	// the deploy.yaml, cloning the repo, or knowing the internal spec shape —
	// the node stays the single owner of what it runs.
	//   POST /v1/image   body: {"service":"web","image":"registry/ns/web:tag"}
	RouteImage = "/v1/image"
	// RouteGatewayRoutes replaces the node's gateway route set in one shot, the
	// way RouteApply reconciles services: the body carries the WHOLE desired set,
	// not a delta, and the handler converges the live front door toward it. PUT
	// only, so a partial write cannot be mistaken for an append.
	//   PUT /v1/gateway/routes   body: {"routes":[...]}
	RouteGatewayRoutes = "/v1/gateway/routes"
	// RouteGateway reports the host gateway singleton's state (driver, running,
	// listeners). Node-scoped like RouteSpaces: the singleton fronts every
	// space, so no space prefix applies. GET only. 503 when no gateway control
	// is installed (the node opened no gateway yet).
	RouteGateway = "/v1/gateway"
	// RouteGatewayUp (re-)ensures the host gateway singleton, idempotently: a
	// live gateway is a no-op, a dead one is relaunched (fresh boot = missing
	// ACME certs are retried). It is the remote `sklp gateway up`. POST only.
	RouteGatewayUp = "/v1/gateway/up"
	// RouteGatewayCerts reports the TLS certificate each routed host is served,
	// read by dialling the front door itself rather than by parsing a driver's
	// cert store: the handshake is what a visitor gets, it answers for either
	// front door (traefik or the native L7), and it never opens a file holding
	// private keys. Node-scoped like RouteGateway. GET only.
	RouteGatewayCerts = "/v1/gateway/certs"
	// RouteSpaces lists every space on the NODE, not just the one this
	// supervisor owns. It is the only node-scoped route: every other route here
	// is bound to the mux's single space. A node serves it so an operator view
	// (the cockpit, the skalpai UI) can enumerate what a machine runs without
	// knowing a space name up front — and keeps answering when the space this
	// supervisor owns is gone. GET only.
	RouteSpaces = "/v1/spaces"

	// RouteTokenRotate (POST, node-scoped) replaces the listener's Bearer token
	// and returns the new one once; the token that authenticated the call is
	// dead when the response is written.
	RouteTokenRotate = "/v1/token/rotate"

	// RouteManifest serves the DECLARED manifest of the space, the read
	// counterpart of RouteApply: the node owns what it was told to run, so an
	// operator view reads it here instead of re-deriving it from a deploy.yaml
	// it would have to clone the repo to see. Secret-free by construction — the
	// persisted manifest has every ${VAR} value stripped. GET only.
	RouteManifest = "/v1/manifest"

	// RouteMeshPeers is node-scoped: GET lists the peers meshauthority knows,
	// DELETE RouteMeshPeers/<id> forgets one. The node relays with its own
	// mesh token, so an operator never needs the authority's port or secret.
	RouteMeshPeers = "/v1/mesh/peers"

	// RouteDial opens a raw TCP stream to a live instance of a service, on
	// its declared port only: POST /v1/dial/<svc>. The reply is a 101 and every
	// byte after it is the socket, both ways. It is what lets a reader off the
	// node speak a database's own protocol without the database ever being
	// reachable from outside.
	RouteDial = "/v1/dial/"

	// RouteDataStores lists the space's stores — services keeping data in a
	// named volume — with how well each is protected. Secret-free: it names
	// the backup destination, never its credentials. GET only.
	RouteDataStores = "/v1/datastores"

	// RouteRestores (node-scoped, GET) lists every restore target the node
	// drills, each with its report history, newest first.
	RouteRestores = "/v1/restores"

	// RoutePublicNIC (node-scoped, GET) reports the public NIC reconciler:
	// desired and observed policy routing, and the last repair.
	RoutePublicNIC = "/v1/host/public-nic"
)

// SpaceRoutePrefix returns the "/v1/spaces/<space>" path prefix a multi-space
// network node (ServeNetMulti) dispatches on: every space-scoped route
// (RouteApply, RouteState, RouteLogs, ...) is served there instead of at its
// bare (single-space) path. The LOCAL Unix-socket transport never uses this —
// SocketPath(space) already identifies the space by which file was dialed, so
// the single-space routes stay unprefixed there. RouteSpaces itself is never
// prefixed: it is node-scoped, answered once regardless of which (or whether
// any) space-scoped path a request also names.
func SpaceRoutePrefix(space string) string {
	return RouteSpaces + "/" + space
}

// SpacesResponse is the GET /v1/spaces body: the node's whole space inventory.
// Spaces carries spaceapi.SpaceInfo values encoded by the Inventory seam, kept
// as RawMessage so this build-tag-free file never references the embedded-only
// spaceapi types (same reason ApplyRequest.Spec is opaque).
type SpacesResponse struct {
	// Node identifies the machine that answered, so a multi-node view can label
	// rows without trusting the address it dialled.
	Node   string          `json:"node"`
	Spaces json.RawMessage `json:"spaces"`
}

// ImageRequest is the POST /v1/image body: roll Service onto Image, leaving the
// rest of the persisted manifest untouched.
type ImageRequest struct {
	Service string `json:"service"`
	Image   string `json:"image"`
}

// GatewayRoutesRequest is the PUT /v1/gateway/routes body: the complete route
// set the node should serve. An empty (or absent) Routes clears every route —
// that is the documented way to take the front door down, not a malformed
// request.
type GatewayRoutesRequest struct {
	Routes []spacemanager.ResolvedRoute `json:"routes"`
}

// GatewayRoutesResponse reports what the node converged to. Changed is false
// when the desired set already matched the live one, so a caller reconciling on
// a timer can tell a real convergence from a no-op.
type GatewayRoutesResponse struct {
	Changed bool `json:"changed"`
	Routes  int  `json:"routes"`
}

// GatewayCertsResponse is the GET RouteGatewayCerts body: one entry per host
// the node routes, deduplicated.
type GatewayCertsResponse struct {
	Certs []GatewayCert `json:"certs"`
}

// GatewayCert is the certificate one host is served. Error is set when the
// handshake itself failed, and the other fields are then zero: a host that
// does not answer is a different state from one served a bad certificate, and
// the UI has to tell them apart.
type GatewayCert struct {
	Host string `json:"host"`
	// Issuer is the CA common name ("R3", "E6"), empty on a self-signed cert.
	Issuer string `json:"issuer,omitempty"`
	// SelfSigned marks the fallback certificate a front door serves when ACME
	// never issued for this host — the shape a missing route produces.
	SelfSigned bool      `json:"self_signed,omitempty"`
	NotAfter   time.Time `json:"not_after,omitempty"`
	Error      string    `json:"error,omitempty"`
}

// ExecRequest is the POST /v1/exec/<svc> JSON body: the command to run inside the
// service's live instance. An empty Cmd defaults to ["sh"] server-side.
type ExecRequest struct {
	Cmd []string `json:"cmd,omitempty"`
}

// HeaderExecExitCode carries the exec'd process exit code back on the NON-tty
// path (the body holds the merged stdout/stderr stream). The client reads it as
// its own exit code. The tty path cannot use a trailer (the connection is
// hijacked), so it closes the stream when the exec ends — best-effort exit code.
const HeaderExecExitCode = "X-Sklp-Exit-Code"

// Rollout result values carried by RolloutState.Result. They mirror the
// supervisor's own constants (spacemanager/rollout.go), restated here so a
// client can read them without the embedded-only package: "rolling" and
// "soaking" are in flight, the rest are terminal.
const (
	RolloutRolling    = "rolling"
	RolloutSoaking    = "soaking"
	RolloutSucceeded  = "succeeded"
	RolloutRolledBack = "rolled_back"
	RolloutSuperseded = "superseded"
)

// Apply result codes (kubectl-style outcome of a reconcile).
const (
	// ResultUnchanged: the desired manifest matched the live one; no mutation.
	ResultUnchanged = "unchanged"
	// ResultConfigured: the live supervisor was reconciled toward the manifest.
	ResultConfigured = "configured"
	// ResultCreated: the space was brought up by this apply (first deploy).
	// Reserved for the J3 create flow; J2 re-apply only yields unchanged/configured.
	ResultCreated = "created"
	// ResultRejected: at least one service's spec is the one its last rollout
	// rolled back, and the supervisor refused to replay it. Nothing of that
	// service moved; Changes names it with the rollback's reason.
	ResultRejected = "rejected"
)

// SocketPath is the per-space control socket path. It lives under
// $XDG_RUNTIME_DIR/sklp/<space>.sock when XDG_RUNTIME_DIR is set (the
// canonical per-user runtime dir, tmpfs, 0700), else falls back to
// <cacheRoot>/run/sklp/<space>.sock. The directory is created 0700 by the
// server before bind; this function is pure (it does not touch the FS).
func SocketPath(space string) string {
	return filepath.Join(socketDir(), space+".sock")
}

// socketDir is the parent directory of every space socket.
func socketDir() string { return xdg.RuntimeRunRoot() }

// ListSpaces returns the names of the spaces with a LIVE control socket on this
// host — the multi-space view `sklp deploy ps` (with no -space) fans out across.
// It scans the socket dir for <space>.sock entries and keeps only those whose
// socket actually answers (a crashed supervisor leaves a stale node, skipped
// here and reaped on the next Listen). Names are returned sorted; a missing
// socket dir yields an empty list, not an error.
func ListSpaces() ([]string, error) {
	dir := socketDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var spaces []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sock") {
			continue
		}
		if !socketResponds(filepath.Join(dir, name)) {
			continue
		}
		spaces = append(spaces, strings.TrimSuffix(name, ".sock"))
	}
	sort.Strings(spaces)
	return spaces, nil
}

// socketResponds reports whether a unix socket has a live listener. Kept here
// (not the embedded-only server.socketAlive) so ListSpaces works in the default
// build too — net.DialTimeout is portable.
func socketResponds(socket string) bool {
	c, err := net.DialTimeout("unix", socket, 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// StateResponse is the GET /v1/state body: the supervisor's view of the whole
// space — the single source of truth `sklp deploy ps` renders.
type RestoresResponse struct {
	Node    string          `json:"node"`
	Targets json.RawMessage `json:"targets"`
}

type PublicNICResponse struct {
	Node   string          `json:"node"`
	Status json.RawMessage `json:"status"`
}

type DataStoresResponse struct {
	Space  string                   `json:"space"`
	Stores []spacemanager.DataStore `json:"stores"`
}

type StateResponse struct {
	Space    string         `json:"space"`
	AllReady bool           `json:"all_ready"`
	Services []ServiceState `json:"services"`
}

// ServiceState is one service's aggregate state plus its live instances.
type ServiceState struct {
	Name  string `json:"name"`
	State string `json:"state"`

	// Image is the OCI reference the service's desired manifest names, so a
	// caller learns what a service runs without holding that manifest — an
	// image watcher derives the repository to poll from it.
	Image string `json:"image,omitempty"`

	// Oneshot marks a job: a service is expected to stay up, a job is expected
	// to exit. A caller cannot infer it from State alone (a stopped service and
	// a completed job both have no live process), and only a job can be re-run.
	Oneshot   bool            `json:"oneshot,omitempty"`
	Instances []InstanceState `json:"instances"`
	// Rollouts is the service's recent rolling updates and how each ended, the
	// in-flight one last. A caller reads it to tell an image that was applied
	// from one that is serving.
	Rollouts []RolloutState `json:"rollouts,omitempty"`

	// Runs is a job's recent executions, oldest last. Without it a completed
	// job reports no clock, and the container log holds only the last run
	// (it is reopened O_TRUNC on every launch) — so two runs printing the
	// same line cannot be told apart.
	Runs []JobRunState `json:"runs,omitempty"`

	// RejectedReason is set when the service holds a spec whose rollout was
	// rolled back: the supervisor refuses to replay it, so every apply of that
	// spec is a silent no-op and the service stays frozen on its previous
	// definition. Without it that refusal is visible only in the reply to the
	// apply that hit it, and a service stuck this way reads as merely idle.
	RejectedReason string `json:"rejected_reason,omitempty"`
}

// JobRunState is one execution of a oneshot job.
type JobRunState struct {
	ID         string    `json:"id"`
	Instance   string    `json:"instance"`
	Image      string    `json:"image,omitempty"`
	Trigger    string    `json:"trigger,omitempty"`
	ExitCode   int       `json:"exit_code"`
	Failed     bool      `json:"failed,omitempty"`
	LogTail    []string  `json:"log_tail,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}

// RolloutState is one rolling update of a service.
type RolloutState struct {
	ID         string    `json:"id"`
	FromImage  string    `json:"from_image"`
	ToImage    string    `json:"to_image"`
	Result     string    `json:"result"`
	Reason     string    `json:"reason,omitempty"`
	ExitCode   int       `json:"exit_code,omitempty"`
	LogTail    []string  `json:"log_tail,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}

// InstanceState is one running (or scheduled-to-restart) replica.
type InstanceState struct {
	ID    string `json:"id"`
	State string `json:"state"`
	PID   int    `json:"pid"`

	// IP/Port are the instance's real container address and declared port:,
	// when the launcher can resolve them. Empty/0 otherwise. The Network
	// Controller (spacemanager/netctrl) polls GET /v1/state and reads these
	// to learn a service's reachable endpoint, entirely independent of
	// space.Space.
	IP   string `json:"ip,omitempty"`
	Port int    `json:"port,omitempty"`

	// Restarts is how many times the instance came back after its first run,
	// and StartedAt when the current run began. This state is a snapshot, so a
	// service that dies and returns inside a poll interval reads as plain
	// ready: these two are what separate that flap from a service that has
	// genuinely been up, and neither can be derived from the rest.
	//
	// Both timestamps are pointers because encoding/json never omits a struct:
	// a zero time.Time ships as "0001-01-01T00:00:00Z", which a reader cannot
	// tell from a real instant, and which is emphatically not "absent".
	Restarts  int        `json:"restarts,omitempty"`
	StartedAt *time.Time `json:"started_at,omitempty"`

	// RestartAt is when a dead instance is due back, absent when no restart is
	// scheduled. BackoffMS is the delay the next one would wait: it climbs
	// 1s→30s while a service keeps dying and resets after a stable run, so a
	// backoff near the ceiling is a crash loop stated outright.
	RestartAt *time.Time `json:"restart_at,omitempty"`
	BackoffMS int64      `json:"backoff_ms,omitempty"`

	// JobFails counts a oneshot's non-zero exits; Held marks one that spent
	// its retry cap. A held job is terminal — never relaunched, and AllReady
	// stays false for as long as it is declared — yet it carries the same
	// "failed" state as a job that has only just missed once.
	JobFails int  `json:"job_fails,omitempty"`
	Held     bool `json:"held,omitempty"`
}

// ApplyRequest is the POST /v1/apply body: the resolved desired manifest of a
// space. Spec carries the marshalled []deploy.Service + SpaceMeta as opaque
// JSON so this DTO (and the client) stay compilable WITHOUT the embedded_runtime
// tag — only the gated server decodes Spec into deploy types. The client builds
// Spec via MarshalApplySpec (also gated) and posts it verbatim.
type ApplyRequest struct {
	// Spec is the gated payload: a JSON object {"services":[...],"meta":{...}}.
	// Kept as RawMessage so neither protocol.go nor client.go references the
	// embedded-only deploy.Service / deploy.SpaceMeta types.
	Spec json.RawMessage `json:"spec"`
	// Force adopts a space already owned by a DIFFERENT stack instead of
	// refusing the apply. Mirrors the local `sklet apply --force` guard
	// (apps/sklp/sklet/reapply.go TryReapply) on the network path, which had
	// none: without it, a remote `sklp deploy --server` silently reconciled
	// away another stack's live services with no error and no confirmation.
	Force bool `json:"force,omitempty"`
	// Source, when set, replaces Spec: the node resolves the raw deploy.yaml
	// itself with the given ${VAR} values, exactly as `sklp deploy` does locally.
	Source *ApplySource `json:"source,omitempty"`
}

// ApplySource is a deploy.yaml the node has never seen, with every ${VAR}
// it references already resolved by the sender.
type ApplySource struct {
	YAML  string            `json:"yaml"`
	Vars  map[string]string `json:"vars,omitempty"`
	Stack string            `json:"stack,omitempty"`
}

// ApplyResult is the POST /v1/apply response: the kubectl-style outcome plus the
// per-service change lines (e.g. "added api", "reconfigured web", "removed old").
type ApplyResult struct {
	// Result is one of ResultUnchanged / ResultConfigured / ResultCreated /
	// ResultRejected.
	Result string `json:"result"`
	// Changes is the human-readable list of mutations applied; empty when
	// Result is unchanged.
	Changes []string `json:"changes,omitempty"`
	// Warnings are conditions the apply accepted but the operator should see at
	// the moment they deploy — a database nothing backs up, say. They never fail
	// the apply; an older client that ignores the field loses nothing but the
	// message. Empty is the normal case.
	Warnings []string `json:"warnings,omitempty"`
}

// DownRequest is the optional POST /v1/down body. An absent body means Purge
// (the historical behaviour of this route, which only ever served
// `sklp deploy delete`), so every existing caller keeps working.
type DownRequest struct {
	// Purge removes the space's persisted manifest and cache once it has
	// stopped, so it does not come back at the next boot — a deletion. Left
	// false the space is merely STOPPED: its containers go, its manifest and
	// volumes stay, and it can be started again from what is on disk.
	Purge bool `json:"purge"`

	// Volumes drops the space's named volumes along with the rest — the data
	// itself, irreversibly. Only read when Purge is set. Left false the purge
	// keeps them, which is what kept the space dir alive whenever the volumes
	// lived under it (the legacy no-data-root layout): the inventory lists
	// spaces by directory name, so the deleted space reappeared as a stopped
	// ghost no command could remove.
	Volumes bool `json:"volumes"`
}

// DownResponse is the POST /v1/down body: a 202 ack sent BEFORE the supervisor
// blocks on its teardown, so the client never times out waiting for the full
// stop. After this ack the caller polls the socket disappearing to confirm.
type DownResponse struct {
	Stopping bool `json:"stopping"`
}

// StartResponse is the POST /v1/start body: whether the space had to be
// brought up, or was already running.
type StartResponse struct {
	Started bool   `json:"started"`
	Space   string `json:"space"`
}

// HealthResponse is the GET /v1/health body: a liveness ping for the
// supervisor, used by the CLI to tell "deployed" from "not deployed".
type HealthResponse struct {
	Space string `json:"space"`
	PID   int    `json:"pid"`
	// Since is the supervisor start time, RFC3339.
	Since string `json:"since"`
}

// ManifestResponse is the GET /v1/spaces/<name>/manifest body: the DECLARED
// shape of a space (what it was told to run), the counterpart of StateResponse
// (what it is actually running). Unlike ApplyRequest.Spec it is not opaque:
// this is a read surface for operators, so the fields are named on the wire
// rather than carried as a marshalled internal struct whose Go field names
// would leak into the API.
type ManifestResponse struct {
	Space  string          `json:"space"`
	Stack  string          `json:"stack,omitempty"`
	URL    string          `json:"url,omitempty"`
	Routes []ManifestRoute `json:"routes,omitempty"`
	// Services and Jobs are split on the wire because they answer different
	// questions: a service is expected to stay up, a job is expected to exit.
	// The persisted manifest keeps both in one slice, told apart by Oneshot.
	Services []ManifestService `json:"services"`
	Jobs     []ManifestService `json:"jobs,omitempty"`
}

// ManifestRoute is one public gateway route of the space.
type ManifestRoute struct {
	Service string `json:"service"`
	Host    string `json:"host,omitempty"`
	Path    string `json:"path,omitempty"`
}

// ManifestService is one declared service or job.
type ManifestService struct {
	Name      string             `json:"name"`
	Image     string             `json:"image,omitempty"`
	Command   []string           `json:"command,omitempty"`
	Replicas  int                `json:"replicas,omitempty"`
	Port      int                `json:"port,omitempty"`
	Ports     []string           `json:"ports,omitempty"`
	Volumes   []string           `json:"volumes,omitempty"`
	DependsOn []string           `json:"depends_on,omitempty"`
	Schedule  string             `json:"schedule,omitempty"`
	Env       []ManifestEnv      `json:"env,omitempty"`
	Resources *ManifestResources `json:"resources,omitempty"`
	Ready     *ManifestReady     `json:"ready,omitempty"`
	Backup    *ManifestBackup    `json:"backup,omitempty"`
}

// ManifestEnv is one environment entry, classified so a reader never has to
// guess whether an empty value means "unset" or "held elsewhere".
//
// Source is one of:
//   - "reference": the value came from one or more ${VAR}; Ref names them
//     (comma-separated) and Value is empty for a whole-value ref or the raw
//     template — placeholders, never secrets — for a composed one. The real
//     values live in the space's 0600 env file, which this API never reads.
//   - "literal": the value was written inline in the deploy.yaml and is
//     returned as-is.
//   - "sensitive": a literal whose KEY looks secret (the same heuristic that
//     warns at deploy time). Value is returned so an operator can still audit
//     what their own manifest inlined, and the label is what tells the UI to
//     mask it by default.
type ManifestEnv struct {
	Key    string `json:"key"`
	Value  string `json:"value,omitempty"`
	Ref    string `json:"ref,omitempty"`
	Source string `json:"source"`
}

// Env source values.
const (
	EnvSourceReference = "reference"
	EnvSourceLiteral   = "literal"
	EnvSourceSensitive = "sensitive"
)

// ManifestResources are the declared cgroup caps, in the units the operator
// wrote them (cores, bytes) rather than the kernel's.
type ManifestResources struct {
	CPUCores    float64 `json:"cpu_cores,omitempty"`
	MemoryBytes int64   `json:"memory_bytes,omitempty"`
	PidsMax     int64   `json:"pids_max,omitempty"`
	Require     bool    `json:"require,omitempty"`
}

// ManifestReady is the declared readiness probe; exactly one of the first four
// fields is set.
type ManifestReady struct {
	HTTP    string `json:"http,omitempty"`
	TCP     string `json:"tcp,omitempty"`
	Log     string `json:"log,omitempty"`
	File    string `json:"file,omitempty"`
	Timeout string `json:"timeout,omitempty"`
}

// ManifestBackup is the declared backup command of a service.
type ManifestBackup struct {
	Schedule string   `json:"schedule,omitempty"`
	Command  []string `json:"command,omitempty"`
}

type TokenRotateResponse struct {
	Token string `json:"token"`
}

// MeshPeersResponse answers GET RouteMeshPeers.
type MeshPeersResponse struct {
	Peers []MeshPeer `json:"peers"`
}

// MeshPeer is one node of the WireGuard mesh as meshauthority lists it.
type MeshPeer struct {
	ID         string     `json:"id"`
	SignKey    string     `json:"sign_key,omitempty"`
	PrivateIP  string     `json:"private_ip"`
	Endpoint   string     `json:"endpoint,omitempty"`
	Spaces     []string   `json:"spaces,omitempty"`
	Services   int        `json:"services"`
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
}
