package runserve

import (
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/lalternativefabrique/sklp/pipeline"
	"github.com/lalternativefabrique/sklp/pipeline/spec"
)

// Server executes pipelines on behalf of a remote caller. WorkRoot is where
// each run's clone lands; an empty value uses the OS temp dir.
type Server struct {
	Token    string
	WorkRoot string
	Version  string
	// Concurrency caps how many runs execute at once on this node. <= 1 keeps
	// the historical one-at-a-time behaviour; higher values let independent
	// pipelines (different repos, or unrelated tasks on the same repo) run
	// side by side instead of queuing behind each other. Each run still gets
	// its own clone/workdir, so concurrent runs do not share mutable state —
	// the limit exists only to bound how many builds compete for this node's
	// CPU/cache at once.
	Concurrency int
	// AllowHostSteps lets a `space: false` pipeline run on this node. Off by
	// default: the YAML comes from the ref under test, so a PR could otherwise
	// leave the sandbox and read this node's token and every run's secrets.
	AllowHostSteps bool

	// runs gates how many pipelines execute at once, sized to Concurrency by
	// initRuns. Queuing past the limit is the caller's job — a request beyond
	// the available slots waits rather than being refused, so a burst of PRs
	// does not drop runs on the floor.
	runs     chan struct{}
	runsOnce sync.Once
	// inflight counts the runs accepted and not yet finished, the ones
	// waiting on runs included.
	inflight atomic.Int32

	regOnce sync.Once
	reg     *registry
}

func (s *Server) registry() *registry {
	s.regOnce.Do(func() { s.reg = newRegistry() })
	return s.reg
}

// Handler is the API's mux, token-gated. Exposed separately from Serve so
// tests can drive it over httptest without TLS.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(RouteHealth, s.handleHealth)
	mux.HandleFunc(RouteRun, s.handleRun)
	mux.HandleFunc(RouteRunStatus, s.handleRunRoute)
	mux.HandleFunc(RouteCacheClean, s.handleCacheClean)
	return tokenAuth(s.Token, mux)
}

// Serve runs the API on addr over TLS until the listener fails. Unlike the
// deploy control API, a bind failure here IS fatal: a CI node that cannot be
// reached has no other purpose to fall back on.
func (s *Server) Serve(addr string, cert tls.Certificate) error {
	tcp, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	ln := tls.NewListener(tcp, &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	})
	srv := &http.Server{Handler: s.Handler()}
	// A node being replaced (a rollout, a drain) must finish the run it is
	// streaming rather than cut the caller off with an EOF: stop accepting,
	// then wait for the in-flight response up to the grace period the
	// orchestrator gives us.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()
	err = srv.Serve(ln)
	if !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	// Serve returns the moment Shutdown closes the listener, while Shutdown
	// itself is still waiting for the run being streamed. Returning here would
	// let main exit and cut that stream; wait for the drain to finish.
	<-drained
	return nil
}

const drainTimeout = 10 * time.Minute

// initRuns sizes the concurrency gate on first use, so a Server built as a
// bare struct literal (every production call site, plus every existing test)
// keeps the historical one-at-a-time behaviour without having to set
// Concurrency explicitly.
func (s *Server) initRuns() {
	s.runsOnce.Do(func() {
		n := s.Concurrency
		if n < 1 {
			n = 1
		}
		s.runs = make(chan struct{}, n)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, HealthResponse{Version: s.Version, Ready: true, Running: int(s.inflight.Load())})
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req RunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "malformed request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.RepoURL == "" || req.Task == "" || req.RunID == "" {
		http.Error(w, "repo_url, task and run_id are required", http.StatusBadRequest)
		return
	}

	// The status line commits before the first frame, so every outcome past
	// this point — including a failure to clone — is reported as an "error"
	// frame on a 200 stream rather than an HTTP error the caller can no
	// longer receive.
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)

	record := s.registry().start(req.RunID)
	enc := &frameWriter{w: w, flusher: flusherOf(w), record: record}

	// Say where the run stands before any work: a caller watching a run that
	// waits behind another one on this node otherwise sees nothing at all.
	ahead := int(s.inflight.Add(1)) - 1
	defer s.inflight.Add(-1)
	if ahead > 0 {
		enc.emit(FrameQueued, strconv.Itoa(ahead))
	}
	s.initRuns()
	s.runs <- struct{}{}
	defer func() { <-s.runs }()
	enc.emit(FrameStarted, s.nodeName())

	workDir, err := os.MkdirTemp(s.WorkRoot, "sklp-run-")
	if err != nil {
		enc.emit(FrameError, "create workspace: "+err.Error())
		return
	}
	defer os.RemoveAll(workDir)

	repoDir := filepath.Join(workDir, "repo")
	if err := pipeline.SelfClone(repoDir, req.RepoURL, req.HeadRef, req.BaseRef); err != nil {
		// The URL may embed a token; SelfClone redacts it in its own errors,
		// but a wrapped one could still carry it, so scrub again here.
		enc.emit(FrameError, "clone: "+redact(err.Error(), req.RepoURL))
		return
	}

	// Through ParseArgsAndRunWithOutput, not Run: the decision to execute in a
	// Space — and the writable cache and HOME that come with it — is made
	// there. Calling Run directly ran pipelines straight on the node's
	// filesystem, where a toolchain writing to $HOME (cargo) hit a read-only
	// path and failed in a way that never happens on a developer machine.
	taskPath := filepath.Join(".sklp", "pipelines", req.Task+".yaml")
	if err := s.checkHostSteps(filepath.Join(repoDir, taskPath)); err != nil {
		enc.emit(FrameError, err.Error())
		return
	}
	args := []string{
		"-task", taskPath,
		"-repo", repoDir,
		"-output", "json",
	}

	// req.Secrets must also reach a spawned Space/runc container, which sees
	// none of this process's env: spawn.Config.EnvFile is the only channel
	// in. Write them to a 0600 file scoped to workDir (removed with it) and
	// pass it the same way a caller's own -env-file would. A `space: false`
	// pipeline picks up the same file below via its own subprocess env.
	if len(req.Secrets) > 0 {
		secretsFile := filepath.Join(workDir, "secrets.env")
		var b strings.Builder
		for _, k := range sortedKeys(req.Secrets) {
			b.WriteString(k)
			b.WriteByte('=')
			b.WriteString(req.Secrets[k])
			b.WriteByte('\n')
		}
		if err := os.WriteFile(secretsFile, []byte(b.String()), 0o600); err != nil {
			enc.emit(FrameError, "write secrets: "+err.Error())
			return
		}
		args = append(args, "-env-file", secretsFile)
	}
	if req.BaseRef != "" {
		args = append(args, "-base-ref", req.BaseRef)
	}
	if req.Provider.Kind != "" {
		args = append(args, "-provider", req.Provider.Kind)
	}
	if req.Provider.Model != "" {
		args = append(args, "-model", req.Provider.Model)
	}
	if req.Provider.BaseURL != "" {
		args = append(args, "-base-url", req.Provider.BaseURL)
	}

	// NOT r.Context(): that one is cancelled the moment the client goes away,
	// and every exec in the pipeline runs under CommandContext — a caller
	// dropping the connection would kill a build mid-step, surfacing as
	// whatever the killed process happened to be doing. The run is owned by the
	// node once it starts; the caller can stop watching without stopping it.
	runCtx, cancelRun := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancelRun()
	record.setCancel(cancelRun)

	// Every run executes as its own subprocess (re-exec of this binary), so
	// the env carrying its secrets is local to that child. The Space regime
	// used to run in-process behind a whole-run env lock (secrets could only
	// enter via os.Setenv, and two tenants sharing one process env table is a
	// leak) — which quietly serialized EVERY secret-bearing run and made
	// --concurrency N a fiction: a node showed one pipeline advancing while
	// the rest queued. The subprocess needs no lock at all, for either regime.
	rawReport, runErr := s.runSubprocess(runCtx, repoDir, args, req, enc)
	if rawReport != "" {
		enc.emit(FrameReport, rawReport)
	}
	if runErr != nil {
		// A cancelled run is killed mid-step, so runErr is whatever the dying
		// subprocess reported ("signal: killed"). Say what actually happened
		// instead: the caller asked for this, and the step's error is noise.
		if record.wasCancelled() {
			enc.emit(FrameError, "run cancelled")
		} else {
			enc.emit(FrameError, runErr.Error())
		}
	}
	if rawReport == "" && runErr == nil {
		// Neither branch above ran: emit carries this to the record too, so
		// finish it directly instead of duplicating that logic here.
		record.finish(StatusError, "", "run finished with neither a report nor an error")
	}
}

// runSubprocess executes a pipeline as its own OS subprocess (re-exec'ing
// this same sklp binary) for BOTH regimes: a `space: false` pipeline runs on
// the host from the child, everything else spawns its Space/runc container
// from the child exactly as the in-process path did. The child gets its own
// env copy carrying the run's secrets (RunPipeline resolves `secrets:`, the
// stack config's fromHost check and provider keys from os.Environ(); the
// -env-file already in args feeds the Space spawn), so concurrent runs share
// nothing to serialize on — the whole-run env lock the in-process path needed
// is gone with the path.
func (s *Server) runSubprocess(runCtx context.Context, repoDir string, args []string, req RunRequest, enc *frameWriter) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve sklp binary: %w", err)
	}

	// "run" here is the top-level CLI verb (cmd/sklp/main.go), which parses
	// its own -file/positional-name flag before handing off to the pipeline
	// runner — it does not understand -task, the runner's own flag. args
	// carries "-task <path>" because that is what the in-process path
	// (pipeline.ParseArgsAndRunWithOutput, called directly) expects; re-exec
	// through the CLI needs -file instead, or the CLI's own arg parser reads
	// the path as a bare task *name* and appends ".yaml" again.
	cliArgs := make([]string, len(args))
	copy(cliArgs, args)
	for i, a := range cliArgs {
		if a == "-task" {
			cliArgs[i] = "-file"
		}
	}
	cmd := exec.CommandContext(runCtx, exe, append([]string{"run"}, cliArgs...)...)
	cmd.Dir = repoDir
	cmd.Env = hostExecEnv(req)
	// Its own process group, so cancelling reaches the whole run and not just
	// this child: a pipeline step is itself a tree (a shell, a compiler, a
	// runc container), and CommandContext's default kill signals the direct
	// child alone — the descendants keep the node busy with work nobody is
	// waiting for any more.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// stdout carries only the final `-output json` report (ParseArgsAndRun
	// writes it there); stderr carries the live log stream. Keeping them on
	// separate pipes is what lets the report be parsed standalone below
	// instead of scraped out of the interleaved log frames.
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = enc.sink(FrameLog)

	runErr := cmd.Run()
	return strings.TrimSpace(stdout.String()), runErr
}

// hostExecEnv builds the subprocess env for runSubprocess. req.Secrets
// must land here, not via os.Setenv on this process: RunPipeline (run inside
// the subprocess) resolves `secrets:` and the stack config's fromHost check
// straight from its own os.Environ(), with no injected-secrets channel of its
// own (see resolveSecrets, LoadPipelineStackConfig). -env-file in args only
// reaches the Space spawn path; a subprocess never reads its own argv back as
// env. Building it here rather than via os.Setenv on this process is what
// keeps this path lock-free: the returned slice is local to the caller.
func hostExecEnv(req RunRequest) []string {
	env := append(os.Environ(), "SKLP_CI=1")
	for k, v := range req.Secrets {
		env = append(env, k+"="+v)
	}
	if req.Provider.APIKey != "" {
		env = append(env, providerKeyEnv(req.Provider.Kind)+"="+req.Provider.APIKey)
	}
	return env
}

// handleRunStatus answers GET /v1/run/{id}: the run's state as last recorded
// by handleRun, independent of whether the original streaming response is
// still connected. A caller that reconnects after losing the stream (core
// restarted, a network blip) uses this to recover the run instead of either
// re-triggering it or declaring it lost.
// handleRunRoute dispatches /v1/run/{id} by method: GET reads the run's state,
// DELETE stops it.
func (s *Server) handleRunRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		s.handleRunCancel(w, r)
		return
	}
	s.handleRunStatus(w, r)
}

// handleRunCancel stops a run still executing on this node, killing its
// subprocess tree. A run that already finished answers 200 with
// cancelled:false — the caller's intent is satisfied either way, and a run
// this node never heard of is a 404 so a caller can tell the two apart.
func (s *Server) handleRunCancel(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimPrefix(r.URL.Path, RouteRunStatus)
	if runID == "" {
		http.Error(w, "run id is required", http.StatusBadRequest)
		return
	}
	record, ok := s.registry().get(runID)
	if !ok {
		http.Error(w, "unknown run", http.StatusNotFound)
		return
	}
	cancelled := record.requestCancel()
	if cancelled {
		record.finish(StatusCancelled, "", "run cancelled")
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(RunCancelResponse{
		Cancelled: cancelled,
		Status:    record.snapshot().Status,
	})
}

func (s *Server) handleRunStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	runID := strings.TrimPrefix(r.URL.Path, RouteRunStatus)
	if runID == "" {
		http.Error(w, "run id is required", http.StatusBadRequest)
		return
	}
	record, ok := s.registry().get(runID)
	if !ok {
		http.Error(w, "unknown run id: the node has no record of it (never accepted, or pruned after completion)", http.StatusNotFound)
		return
	}
	writeJSON(w, record.snapshot())
}

// handleCacheClean runs `sklp clean` as a subprocess (re-exec'ing this same
// binary, same as runHostExecSubprocess) and returns once it finishes. A
// subprocess rather than an in-process call: cleanCommand's logic lives
// unexported in cmd/sklp and touches process-global state (cgroups, orphan
// PIDs) that has no business running concurrently with a pipeline holding
// envMu. --force skips the confirmation prompt cleanCommand would otherwise
// print to a TTY this request has none of.
func (s *Server) handleCacheClean(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req CacheCleanRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, "malformed request body: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	exe, err := os.Executable()
	if err != nil {
		http.Error(w, "resolve sklp binary: "+err.Error(), http.StatusInternalServerError)
		return
	}
	args := []string{"clean", "--force"}
	if req.Images {
		args = append(args, "--images")
	}
	if req.Deep {
		args = append(args, "--deep")
	}
	cmd := exec.CommandContext(r.Context(), exe, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	runErr := cmd.Run()

	// Also purge the skt toolchain cache (~/.cache/skt): `sklp clean` never
	// touches it (skt owns it), yet a poisoned tool marker there breaks every
	// run on this node the same way a corrupt sklp cache would — the UI's one
	// clean button must reset both. Best-effort through skt's own verb: a node
	// without skt just reports the skip.
	if sktBin, lerr := exec.LookPath("skt"); lerr != nil {
		out.WriteString("skt: not installed, toolchain cache skipped\n")
	} else {
		skt := exec.CommandContext(r.Context(), sktBin, "clean", "--all")
		skt.Stdout = &out
		skt.Stderr = &out
		if serr := skt.Run(); serr != nil {
			out.WriteString("skt clean --all: " + serr.Error() + "\n")
			if runErr == nil {
				runErr = serr
			}
		}
	}

	writeJSON(w, CacheCleanResponse{
		OK:     runErr == nil,
		Output: strings.Split(strings.TrimRight(out.String(), "\n"), "\n"),
	})
}

// sortedKeys returns m's keys in lexical order, so the written env file is
// deterministic across runs.
func sortedKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// redact blanks the userinfo of repoURL wherever it appears in s, so a
// token-bearing clone URL never reaches the caller's logs.
func redact(s, repoURL string) string {
	at := strings.Index(repoURL, "@")
	scheme := strings.Index(repoURL, "://")
	if at < 0 || scheme < 0 || at < scheme {
		return s
	}
	return strings.ReplaceAll(s, repoURL[scheme+3:at], "***")
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func flusherOf(w http.ResponseWriter) http.Flusher {
	f, _ := w.(http.Flusher)
	return f
}

// tokenAuth rejects any request without a matching Bearer token, in constant
// time so the token cannot be guessed byte-by-byte from response timing.
func tokenAuth(token string, h http.Handler) http.Handler {
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := bearerToken(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="sklp run"`)
			http.Error(w, "unauthorized: a valid Bearer token is required", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func bearerToken(authz string) string {
	const prefix = "bearer "
	if len(authz) < len(prefix) || !strings.EqualFold(authz[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(authz[len(prefix):])
}

// providerKeyEnv maps a provider kind to the environment variable the engine
// reads its credential from (see the switch in pipeline.ParseArgsAndRun).
func providerKeyEnv(kind string) string {
	switch kind {
	case "anthropic":
		return "ANTHROPIC_API_KEY"
	case "openai", "openai-compat":
		return "OPENAI_API_KEY"
	case "ovh":
		return "OVH_KEY"
	case "scaleway":
		return "SCW_SECRET_KEY"
	case "deepseek":
		return "DEEPSEEK_API_KEY"
	default:
		return "MISTRAL_API_KEY"
	}
}

// nodeName identifies this node to the caller: the hostname, which on a
// StatefulSet is the pod's stable name and on a VM its machine name.
func (s *Server) nodeName() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "node"
}

func (s *Server) checkHostSteps(pipelinePath string) error {
	if s.AllowHostSteps {
		return nil
	}
	p, err := spec.LoadPipeline(pipelinePath)
	if err != nil {
		return fmt.Errorf("load pipeline: %w", err)
	}
	if p.Space != nil && !*p.Space {
		return fmt.Errorf("pipeline %s sets `space: false` but this node refuses host steps; start the run API with --allow-host-steps to serve trusted refs on the host", filepath.Base(pipelinePath))
	}
	return nil
}
