package runserve

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	s := &Server{Token: "secret-token", Version: "test", WorkRoot: t.TempDir()}
	return httptest.NewServer(s.Handler())
}

func do(t *testing.T, ts *httptest.Server, method, path, token, body string) *http.Response {
	t.Helper()
	var r *http.Request
	var err error
	if body == "" {
		r, err = http.NewRequest(method, ts.URL+path, nil)
	} else {
		r, err = http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	}
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := ts.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestHealthRequiresToken(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	resp := do(t, ts, http.MethodGet, RouteHealth, "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: got %d, want 401", resp.StatusCode)
	}
}

func TestHealthWrongTokenRejected(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	resp := do(t, ts, http.MethodGet, RouteHealth, "wrong-token", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: got %d, want 401", resp.StatusCode)
	}
}

func TestHealthOK(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	resp := do(t, ts, http.MethodGet, RouteHealth, "secret-token", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", resp.StatusCode)
	}
	var h HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		t.Fatal(err)
	}
	if !h.Ready || h.Version != "test" {
		t.Fatalf("unexpected health payload: %+v", h)
	}
}

func TestRunRejectsMissingFields(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	resp := do(t, ts, http.MethodPost, RouteRun, "secret-token", `{"task":"ci"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing repo_url: got %d, want 400", resp.StatusCode)
	}
}

func TestRunRejectsGET(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	resp := do(t, ts, http.MethodGet, RouteRun, "secret-token", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("got %d, want 405", resp.StatusCode)
	}
}

// A clone that cannot succeed must still come back as a stream: the status
// line is already committed when the run starts, so failures are frames.
func TestRunCloneFailureIsAnErrorFrame(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	body := `{"run_id":"r1","repo_url":"https://invalid.invalid/nope.git","task":"ci","head_ref":"main"}`
	resp := do(t, ts, http.MethodPost, RouteRun, "secret-token", body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200 (errors travel as frames)", resp.StatusCode)
	}
	frames := readFrames(t, resp)
	if len(frames) == 0 {
		t.Fatal("no frames emitted")
	}
	last := frames[len(frames)-1]
	if last.Kind != FrameError {
		t.Fatalf("last frame kind = %q, want %q", last.Kind, FrameError)
	}
	if !strings.Contains(last.Text, "clone") {
		t.Fatalf("error frame does not mention the clone: %q", last.Text)
	}
}

// TestRunStatusRecoversAfterStreamRead proves the point of RouteRunStatus: a
// caller that already consumed the stream (or lost it) can still ask the
// node what happened to a specific run_id afterwards.
func TestRunStatusRecoversAfterStreamRead(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	body := `{"run_id":"r-status","repo_url":"https://invalid.invalid/nope.git","task":"ci","head_ref":"main"}`
	resp := do(t, ts, http.MethodPost, RouteRun, "secret-token", body)
	readFrames(t, resp)
	resp.Body.Close()

	statusResp := do(t, ts, http.MethodGet, RouteRunStatus+"r-status", "secret-token", "")
	defer statusResp.Body.Close()
	if statusResp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", statusResp.StatusCode)
	}
	var got RunStatusResponse
	if err := json.NewDecoder(statusResp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusError {
		t.Fatalf("status = %q, want %q", got.Status, StatusError)
	}
	if !strings.Contains(got.Error, "clone") {
		t.Fatalf("error does not mention the clone: %q", got.Error)
	}
}

func TestRunStatusUnknownIDIs404(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	resp := do(t, ts, http.MethodGet, RouteRunStatus+"nope", "secret-token", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("got %d, want 404", resp.StatusCode)
	}
}

func readFrames(t *testing.T, resp *http.Response) []Frame {
	t.Helper()
	var out []Frame
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var f Frame
		if err := json.Unmarshal(line, &f); err != nil {
			t.Fatalf("frame is not valid JSON: %q (%v)", line, err)
		}
		out = append(out, f)
	}
	return out
}

// TestInitRunsDefaultsToOneSlot pins the historical one-at-a-time behaviour
// for a Server built as a bare struct literal (every production call site
// before Concurrency existed, and every other test in this file): the gate
// must hold exactly one slot when Concurrency is unset.
func TestInitRunsDefaultsToOneSlot(t *testing.T) {
	s := &Server{}
	s.initRuns()
	if cap(s.runs) != 1 {
		t.Fatalf("cap(s.runs) = %d, want 1", cap(s.runs))
	}
}

// TestInitRunsHonoursConcurrency checks the gate is sized to Concurrency, and
// that a non-positive value still floors to the safe default of 1.
func TestInitRunsHonoursConcurrency(t *testing.T) {
	s := &Server{Concurrency: 4}
	s.initRuns()
	if cap(s.runs) != 4 {
		t.Fatalf("cap(s.runs) = %d, want 4", cap(s.runs))
	}

	zero := &Server{Concurrency: 0}
	zero.initRuns()
	if cap(zero.runs) != 1 {
		t.Fatalf("cap(zero.runs) = %d, want 1 (Concurrency=0 must not disable the gate)", cap(zero.runs))
	}
}

// TestRunGateAllowsConcurrencyLimitOverlap proves the gate itself allows N
// concurrent holders and blocks the (N+1)th — the actual behaviour change
// this fix delivers, independent of the HTTP handler around it (whose runs
// fail too fast at "clone" to overlap reliably in a timing-based test).
func TestRunGateAllowsConcurrencyLimitOverlap(t *testing.T) {
	s := &Server{Concurrency: 2}
	s.initRuns()

	s.runs <- struct{}{}
	s.runs <- struct{}{}

	select {
	case s.runs <- struct{}{}:
		t.Fatal("a 3rd holder acquired a slot past Concurrency=2")
	default:
	}

	<-s.runs
	select {
	case s.runs <- struct{}{}:
	default:
		t.Fatal("releasing a slot did not free capacity for a new holder")
	}
}

// TestRunCloneFailureStillWritesAndCleansSecretsFile drives handleRun far
// enough to exercise the secrets-file write (the fix for req.Secrets never
// reaching a spawned Space/runc container) and the env cleanup (the fix for
// the cross-run/cross-tenant os.Setenv leak), without depending on a full
// pipeline execution: the clone target is invalid, so the run fails fast
// right after the secrets file would have been consumed, and workDir (with
// it) is removed by the handler's own defer — this test only needs to prove
// the write happened before that point and that no secret survives in the
// process env afterwards.
func TestRunCloneFailureStillWritesAndCleansSecretsFile(t *testing.T) {
	work := t.TempDir()
	s := &Server{Token: "secret-token", Version: "test", WorkRoot: work}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	const secretKey = "S3_SECRET_KEY_TESTONLY"
	os.Unsetenv(secretKey)

	reqBody, err := json.Marshal(RunRequest{
		RunID:   "r-secrets-leak-test",
		RepoURL: "https://invalid.invalid/nope.git",
		Task:    "ci",
		Secrets: map[string]string{secretKey: "from-core"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp := do(t, ts, http.MethodPost, RouteRun, "secret-token", string(reqBody))
	defer resp.Body.Close()

	frames := readFrames(t, resp)
	if len(frames) == 0 || frames[len(frames)-1].Kind != FrameError {
		t.Fatalf("expected the clone to fail with an error frame, got %+v", frames)
	}

	if v, ok := os.LookupEnv(secretKey); ok {
		t.Fatalf("secret leaked into the node process env after the run: %s=%q", secretKey, v)
	}

	entries, err := os.ReadDir(work)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("workDir not cleaned up: %v", entries)
	}
}

// TestHostExecEnvCarriesSecrets pins the fix for `space: false` pipelines
// never receiving req.Secrets: before this, only -env-file carried them, and
// that flag is read solely by the Space spawn path — a host-exec subprocess
// saw none of it, so RunPipeline's fromHost check (LoadPipelineStackConfig)
// treated every declared secret as still needing the vault passphrase, which
// a hosted node has none of. hostExecEnv must put every req.Secrets entry
// directly into the subprocess's own env.
func TestHostExecEnvCarriesSecrets(t *testing.T) {
	req := RunRequest{
		Secrets: map[string]string{"S3_SECRET_KEY": "from-core", "GITHUB_TOKEN": "gh-tok"},
	}
	env := hostExecEnv(req)

	want := map[string]string{"S3_SECRET_KEY": "from-core", "GITHUB_TOKEN": "gh-tok"}
	for k, v := range want {
		entry := k + "=" + v
		found := false
		for _, e := range env {
			if e == entry {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("hostExecEnv did not carry %s into the subprocess env: %v", k, env)
		}
	}
}

func TestHostExecEnvCarriesProviderKey(t *testing.T) {
	req := RunRequest{Provider: Provider{Kind: "anthropic", APIKey: "sk-test"}}
	env := hostExecEnv(req)

	want := providerKeyEnv("anthropic") + "=sk-test"
	for _, e := range env {
		if e == want {
			return
		}
	}
	t.Fatalf("hostExecEnv did not carry the provider key into the subprocess env: %v", env)
}

// TestSecretsFileFormatMatchesLoadDotEnv pins the exact format handleRun
// writes for -env-file: KEY=VALUE lines, one per secret, sorted so the file
// is deterministic. This is the wire format spawn's loadDotEnv parses
// (apps/sklp/pipeline/spawn/oci_embedded_linux.go) — a mismatch here would
// silently drop every secret on the Space/runc path with no error.
func TestSecretsFileFormatMatchesLoadDotEnv(t *testing.T) {
	secrets := map[string]string{"B_KEY": "b-val", "A_KEY": "a-val"}
	var b strings.Builder
	for _, k := range sortedKeys(secrets) {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(secrets[k])
		b.WriteByte('\n')
	}
	want := "A_KEY=a-val\nB_KEY=b-val\n"
	if got := b.String(); got != want {
		t.Fatalf("secrets file content = %q, want %q", got, want)
	}
}

func TestRedactStripsCredentials(t *testing.T) {
	url := "https://x-access-token:ghp_supersecret@github.com/owner/repo.git"
	got := redact("clone failed for "+url, url)
	if strings.Contains(got, "ghp_supersecret") {
		t.Fatalf("token leaked: %q", got)
	}
	if !strings.Contains(got, "***") {
		t.Fatalf("expected redaction marker: %q", got)
	}
}

func TestRunCancelUnknownIDIs404(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	resp := do(t, ts, http.MethodDelete, RouteRunStatus+"nope", "secret-token", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("got %d, want 404", resp.StatusCode)
	}
}

// A run the node already finished answers 200 with cancelled:false — the
// request succeeded, but the caller must not be told it stopped something.
func TestRunCancelOnAFinishedRunStopsNothing(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	body := `{"run_id":"r-done","repo_url":"https://invalid.invalid/nope.git","task":"ci","head_ref":"main"}`
	resp := do(t, ts, http.MethodPost, RouteRun, "secret-token", body)
	readFrames(t, resp)
	resp.Body.Close()

	cancelResp := do(t, ts, http.MethodDelete, RouteRunStatus+"r-done", "secret-token", "")
	defer cancelResp.Body.Close()
	if cancelResp.StatusCode != http.StatusOK {
		t.Fatalf("got %d, want 200", cancelResp.StatusCode)
	}
	var got RunCancelResponse
	if err := json.NewDecoder(cancelResp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Cancelled {
		t.Fatal("a finished run cannot be cancelled")
	}
}

// A cancel arriving before the run armed its stopper must not be lost: the
// record honours it as soon as setCancel hands over the stopper.
func TestRecordHonoursACancelThatArrivedFirst(t *testing.T) {
	r := &runRecord{status: StatusRunning}

	if !r.requestCancel() {
		t.Fatal("a running record must accept a cancel")
	}
	stopped := false
	r.setCancel(func() { stopped = true })
	if !stopped {
		t.Fatal("the stopper must fire for a cancel that arrived before it was armed")
	}
}

func TestRecordCancelStopsTheRun(t *testing.T) {
	r := &runRecord{status: StatusRunning}
	stopped := false
	r.setCancel(func() { stopped = true })

	if !r.requestCancel() || !stopped {
		t.Fatal("requestCancel must run the stopper")
	}
	if !r.wasCancelled() {
		t.Fatal("the record must remember it was cancelled")
	}
	r.finish(StatusCancelled, "", "run cancelled")
	if r.requestCancel() {
		t.Fatal("a finished run must not be cancellable twice")
	}
}

// A run the API launches is CI by definition: the marker selects the pipeline
// `registries:` block over any login file left on the node.
func TestHostExecEnvMarksCI(t *testing.T) {
	for _, kv := range hostExecEnv(RunRequest{}) {
		if kv == "SKLP_CI=1" {
			return
		}
	}
	t.Fatal("hostExecEnv did not set SKLP_CI=1")
}
