package sklpmcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lalternativefabrique/sklp/internal/config"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
	"github.com/lalternativefabrique/sklp/spacemanager/wire"
)

const (
	testToken  = "sklp_ctx_tok_very_secret_0123456789"
	secretVar  = "s3cr3t-db-password-value"
	deployYAML = `env_files: [.env]
services:
  web:
    image: traefik/whoami:v1.10.4
    port: 80
    env:
      DB_PASS: ${DB_PASS}
`
)

type fakeNode struct {
	srv  *httptest.Server
	pin  string
	mu   sync.Mutex
	hits []string
	body map[string][]byte
}

func (f *fakeNode) record(r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits = append(f.hits, r.Method+" "+r.URL.Path)
	if r.Body != nil {
		f.body[r.URL.Path], _ = io.ReadAll(r.Body)
	}
}

func (f *fakeNode) hit(want string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, h := range f.hits {
		if h == want {
			return true
		}
	}
	return false
}

func newFakeNode(t *testing.T) *fakeNode {
	t.Helper()
	f := &fakeNode{body: map[string][]byte{}}
	started := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("GET /v1/spaces", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"node": "demo-node", "spaces": []map[string]any{
			{"name": "rolling-demo", "status": "running", "supervised": true, "containers": []map[string]string{{"id": "a", "status": "running"}}},
			{"name": "other", "status": "stopped", "supervised": true, "repo": "/home/secret/path"},
		}})
	})
	mux.HandleFunc("GET /v1/spaces/rolling-demo/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, control.StateResponse{Space: "rolling-demo", AllReady: true, Services: []control.ServiceState{{
			Name: "web", State: "ready", Image: "traefik/whoami:v1.10.4",
			Instances: []control.InstanceState{{ID: "web-0", State: "ready", PID: 42, IP: "10.0.0.2", Port: 80, StartedAt: &started}},
			Rollouts:  []control.RolloutState{{ID: "r1", FromImage: "traefik/whoami:v1.10.3", ToImage: "traefik/whoami:v1.10.4", Result: "succeeded"}},
		}}})
	})
	mux.HandleFunc("GET /v1/spaces/rolling-demo/manifest", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, control.ManifestResponse{Space: "rolling-demo", Stack: "web", Services: []control.ManifestService{{
			Name: "web", Image: "traefik/whoami:v1.10.4",
			Env: []control.ManifestEnv{
				{Key: "DB_PASS", Ref: "DB_PASS", Source: control.EnvSourceReference},
				{Key: "API_SECRET", Value: "inline-literal-secret", Source: control.EnvSourceSensitive},
				{Key: "LOG_LEVEL", Value: "debug", Source: control.EnvSourceLiteral},
			},
		}}})
	})
	mux.HandleFunc("GET /v1/spaces/rolling-demo/logs/web", func(w http.ResponseWriter, r *http.Request) {
		for i := range 250 {
			fmt.Fprintf(w, "line %d\n", i)
		}
		fmt.Fprintf(w, "leaked %s\n", testToken)
	})
	mux.HandleFunc("GET /v1/spaces/rolling-demo/datastores", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, control.DataStoresResponse{Space: "rolling-demo", Stores: []wire.DataStore{{Service: "db", Engine: wire.EnginePostgres, Protection: wire.ProtectionNone}}})
	})
	mux.HandleFunc("GET /v1/spaces/other/datastores", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusServiceUnavailable)
	})
	mux.HandleFunc("POST /v1/spaces/rolling-demo/apply", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		writeJSON(w, 200, control.ApplyResult{Result: control.ResultConfigured, Changes: []string{"reconfigured web"}, Warnings: []string{"echo " + secretVar}})
	})
	mux.HandleFunc("POST /v1/spaces/rolling-demo/restart/web", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		w.WriteHeader(200)
	})
	mux.HandleFunc("POST /v1/spaces/rolling-demo/run/migrate", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		w.WriteHeader(200)
	})
	mux.HandleFunc("POST /v1/spaces/rolling-demo/down", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		writeJSON(w, http.StatusAccepted, control.DownResponse{Stopping: true})
	})
	mux.HandleFunc("POST /v1/spaces/rolling-demo/exec/web", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		w.Header().Set("Trailer", control.HeaderExecExitCode)
		fmt.Fprintf(w, "hello\ntoken=%s\n", testToken)
		w.Header().Set(control.HeaderExecExitCode, "3")
	})
	mux.HandleFunc("GET /v1/spaces/locked/state", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad token "+testToken, http.StatusUnauthorized)
	})
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	sum := sha256.Sum256(f.srv.Certificate().Raw)
	f.pin = hex.EncodeToString(sum[:])
	return f
}

func writeConfig(t *testing.T, f *fakeNode) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("SKLP_CONFIG", "")
	t.Setenv(config.EnvContext, "")
	t.Setenv(config.EnvContextToken, "")
	t.Setenv(config.EnvToken, "")
	cfg := &config.Config{}
	cfg.SetContext("prod", config.Context{Server: "https://127.0.0.1:1", CAFingerprint: "ab", Token: "prod-token-unrelated"})
	cfg.SetContext("demo", config.Context{Server: f.srv.URL, CAFingerprint: f.pin, Token: testToken, Space: "rolling-demo"})
	path, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
}

func writeRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".sklp", "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".sklp", "web", "deploy.yaml"), []byte(deployYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("DB_PASS="+secretVar+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

func connect(t *testing.T, opts Options) *mcp.ClientSession {
	t.Helper()
	srv, err := NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

type harness struct {
	t    *testing.T
	node *fakeNode
	cs   *mcp.ClientSession
}

func newHarness(t *testing.T, readOnly bool) *harness {
	f := newFakeNode(t)
	writeConfig(t, f)
	return &harness{t: t, node: f, cs: connect(t, Options{Context: "demo", ReadOnly: readOnly})}
}

func (h *harness) call(name string, args map[string]any) (*mcp.CallToolResult, string) {
	h.t.Helper()
	res, err := h.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		h.t.Fatalf("%s: %v", name, err)
	}
	raw, _ := json.Marshal(res)
	for _, leak := range []string{testToken, h.node.pin, secretVar, "inline-literal-secret", "prod-token-unrelated"} {
		if strings.Contains(string(raw), leak) {
			h.t.Fatalf("%s leaked %q: %s", name, leak, raw)
		}
	}
	text := ""
	if len(res.Content) > 0 {
		if tc, ok := res.Content[0].(*mcp.TextContent); ok {
			text = tc.Text
		}
	}
	return res, text
}

func (h *harness) ok(name string, args map[string]any, out any) {
	h.t.Helper()
	res, text := h.call(name, args)
	if res.IsError {
		h.t.Fatalf("%s: tool error: %s", name, text)
	}
	if err := json.Unmarshal([]byte(text), out); err != nil {
		h.t.Fatalf("%s: decode %q: %v", name, text, err)
	}
}

func TestNoContextServesNoNodeEvenWithAnActiveOne(t *testing.T) {
	f := newFakeNode(t)
	writeConfig(t, f)
	t.Setenv(config.EnvContext, "demo")
	cs := connect(t, Options{})
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools.Tools {
		if strings.HasPrefix(tl.Name, "deploy_") && tl.Name != "deploy_contexts" {
			t.Fatalf("%s served without -context", tl.Name)
		}
	}
	if _, err := NewServer(Options{Context: "nope"}); err == nil {
		t.Fatal("an unknown context must be refused")
	}
}

func TestDeployContextsNeverShowsTokensOrFingerprints(t *testing.T) {
	h := newHarness(t, false)
	var out contextsOut
	h.ok("deploy_contexts", nil, &out)
	if out.Bound != "demo" || len(out.Contexts) != 2 {
		t.Fatalf("got %+v", out)
	}
	if out.Contexts[0].Name != "demo" || !out.Contexts[0].Bound || out.Contexts[0].Server != h.node.srv.URL {
		t.Fatalf("demo row = %+v", out.Contexts[0])
	}
	_, text := h.call("deploy_contexts", nil)
	if strings.Contains(text, `"ab"`) {
		t.Fatalf("a fingerprint was listed: %s", text)
	}
}

func TestDeploySpaces(t *testing.T) {
	h := newHarness(t, false)
	var out spacesOut
	h.ok("deploy_spaces", nil, &out)
	if out.Node != "demo-node" || len(out.Spaces) != 2 || out.Spaces[1].Name != "rolling-demo" || out.Spaces[1].Running != 1 {
		t.Fatalf("got %+v", out)
	}
	if _, text := h.call("deploy_spaces", nil); strings.Contains(text, "/home/secret/path") {
		t.Fatalf("the node's host paths were exposed: %s", text)
	}
}

func TestDeployGet(t *testing.T) {
	h := newHarness(t, false)
	var out spaceOut
	h.ok("deploy_get", map[string]any{"space": "rolling-demo"}, &out)
	if !out.AllReady || len(out.Services) != 1 {
		t.Fatalf("got %+v", out)
	}
	web := out.Services[0]
	if web.Image != "traefik/whoami:v1.10.4" || len(web.Instances) != 1 || web.Instances[0].State != "ready" || len(web.Rollouts) != 1 {
		t.Fatalf("web = %+v", web)
	}
	if res, text := h.call("deploy_get", map[string]any{"space": "../etc"}); !res.IsError {
		t.Fatalf("a path-like space name must be refused before any request: %s", text)
	}
	if res, _ := h.call("deploy_get", map[string]any{"space": ""}); !res.IsError {
		t.Fatal("an empty space must be refused")
	}
}

func TestAuthErrorsCarryTheStatusOnly(t *testing.T) {
	h := newHarness(t, false)
	res, text := h.call("deploy_get", map[string]any{"space": "locked"})
	if !res.IsError || text != "the node refused the context's credentials (status 401)" {
		t.Fatalf("got %v %q", res.IsError, text)
	}
}

func TestDeployManifestWithholdsSensitiveValues(t *testing.T) {
	h := newHarness(t, false)
	var out control.ManifestResponse
	h.ok("deploy_manifest", map[string]any{"space": "rolling-demo"}, &out)
	env := out.Services[0].Env
	if len(env) != 3 || env[1].Key != "API_SECRET" || env[1].Value != "" || env[2].Value != "debug" || env[0].Ref != "DB_PASS" {
		t.Fatalf("env = %+v", env)
	}
}

func TestDeployLogsIsBoundedAndRedacted(t *testing.T) {
	h := newHarness(t, false)
	var out logsOut
	h.ok("deploy_logs", map[string]any{"space": "rolling-demo", "service": "web", "lines": 10}, &out)
	if len(out.Lines) != 10 || !out.Truncated || out.Lines[9] != "leaked "+redacted || out.Lines[0] != "line 241" {
		t.Fatalf("got %+v", out)
	}
	h.ok("deploy_logs", map[string]any{"space": "rolling-demo", "service": "web"}, &out)
	if len(out.Lines) != defaultLogLines {
		t.Fatalf("default tail = %d lines", len(out.Lines))
	}
}

func TestDeployDatastores(t *testing.T) {
	h := newHarness(t, false)
	var out dataStoresOut
	h.ok("deploy_datastores", nil, &out)
	if len(out.Spaces) != 2 || out.Spaces[0].Space != "other" || out.Spaces[0].Error == "" || len(out.Spaces[1].Stores) != 1 {
		t.Fatalf("got %+v", out)
	}
	h.ok("deploy_datastores", map[string]any{"space": "rolling-demo"}, &out)
	if len(out.Spaces) != 1 || out.Spaces[0].Stores[0].Engine != wire.EnginePostgres {
		t.Fatalf("got %+v", out)
	}
}

func TestDeployApplySendsTheFileAndItsVars(t *testing.T) {
	h := newHarness(t, false)
	t.Setenv("DB_PASS", "")
	os.Unsetenv("DB_PASS")
	repo := writeRepo(t)
	var out applyOut
	h.ok("deploy_apply", map[string]any{"repo": repo, "stack": "web", "space": "rolling-demo"}, &out)
	if out.Result != control.ResultConfigured || len(out.Changes) != 1 || out.Warnings[0] != "echo "+redacted {
		t.Fatalf("got %+v", out)
	}
	var req control.ApplyRequest
	if err := json.Unmarshal(h.node.body["/v1/spaces/rolling-demo/apply"], &req); err != nil {
		t.Fatal(err)
	}
	if req.Source == nil || req.Source.YAML != deployYAML || req.Source.Vars["DB_PASS"] != secretVar || req.Source.Stack != "web" {
		t.Fatalf("request = %+v", req)
	}
	if _, set := os.LookupEnv("DB_PASS"); set {
		t.Fatal("the repo's env_files stayed in the server's env after the apply")
	}
}

func TestDeployApplyRefusesUnsetVars(t *testing.T) {
	h := newHarness(t, false)
	repo := t.TempDir()
	_ = os.MkdirAll(filepath.Join(repo, ".sklp", "web"), 0o755)
	_ = os.WriteFile(filepath.Join(repo, ".sklp", "web", "deploy.yaml"), []byte("services:\n  web:\n    image: x\n    env:\n      A: ${SKLP_TEST_NEVER_SET}\n"), 0o644)
	res, text := h.call("deploy_apply", map[string]any{"repo": repo, "stack": "web", "space": "rolling-demo"})
	if !res.IsError || !strings.Contains(text, "SKLP_TEST_NEVER_SET") || h.node.hit("POST /v1/spaces/rolling-demo/apply") {
		t.Fatalf("got %v %q", res.IsError, text)
	}
}

func TestDeployExec(t *testing.T) {
	h := newHarness(t, false)
	var out outputOut
	h.ok("deploy_exec", map[string]any{"space": "rolling-demo", "service": "web", "command": []string{"sh", "-c", "env"}}, &out)
	if out.OK || out.ExitCode != 3 || len(out.Output) != 2 || out.Output[0] != "hello" || out.Output[1] != "token="+redacted {
		t.Fatalf("got %+v", out)
	}
	var req control.ExecRequest
	_ = json.Unmarshal(h.node.body["/v1/spaces/rolling-demo/exec/web"], &req)
	if strings.Join(req.Cmd, " ") != "sh -c env" {
		t.Fatalf("cmd = %v", req.Cmd)
	}
}

func TestRestartRunAndDelete(t *testing.T) {
	h := newHarness(t, false)
	var ack ackOut
	h.ok("deploy_restart", map[string]any{"space": "rolling-demo", "service": "web"}, &ack)
	h.ok("deploy_run_job", map[string]any{"space": "rolling-demo", "job": "migrate"}, &ack)
	h.ok("deploy_delete", map[string]any{"space": "rolling-demo", "volumes": true}, &ack)
	if ack.Status != "delete requested; named volumes destroyed" {
		t.Fatalf("ack = %+v", ack)
	}
	for _, want := range []string{"POST /v1/spaces/rolling-demo/restart/web", "POST /v1/spaces/rolling-demo/run/migrate", "POST /v1/spaces/rolling-demo/down"} {
		if !h.node.hit(want) {
			t.Fatalf("node never saw %s", want)
		}
	}
	var down control.DownRequest
	_ = json.Unmarshal(h.node.body["/v1/spaces/rolling-demo/down"], &down)
	if !down.Purge || !down.Volumes {
		t.Fatalf("down = %+v", down)
	}
}

var destructiveTools = []string{"deploy_apply", "deploy_exec", "deploy_restart", "deploy_run_job", "deploy_delete", "space_exec", "space_down"}

var writeTools = []string{"dev_up", "dev_down", "space_stop", "run_pipeline", "job_cancel"}

func TestProtocolEndToEnd(t *testing.T) {
	h := newHarness(t, false)
	initRes := h.cs.InitializeResult()
	if initRes.ServerInfo.Name != "sklp" || initRes.ProtocolVersion == "" {
		t.Fatalf("initialize = %+v", initRes)
	}
	tools, err := h.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*mcp.Tool{}
	for _, tl := range tools.Tools {
		byName[tl.Name] = tl
		if tl.Description == "" || tl.InputSchema == nil {
			t.Fatalf("%s has no description or schema", tl.Name)
		}
	}
	if len(byName) != 25 {
		t.Fatalf("%d tools listed", len(byName))
	}
	for _, name := range writeTools {
		a := byName[name].Annotations
		if a == nil || a.DestructiveHint == nil || *a.DestructiveHint || a.ReadOnlyHint {
			t.Fatalf("%s is not marked as a non-destructive write: %+v", name, a)
		}
	}
	for _, name := range destructiveTools {
		a := byName[name].Annotations
		if a == nil || a.DestructiveHint == nil || !*a.DestructiveHint || a.ReadOnlyHint {
			t.Fatalf("%s is not marked destructive: %+v", name, a)
		}
	}
	if a := byName["deploy_get"].Annotations; a == nil || !a.ReadOnlyHint {
		t.Fatalf("get_space is not read-only: %+v", a)
	}

	var space spaceOut
	h.ok("deploy_get", map[string]any{"space": "rolling-demo"}, &space)
	var ack ackOut
	h.ok("deploy_restart", map[string]any{"space": "rolling-demo", "service": "web"}, &ack)
	if ack.Status != "restart requested" {
		t.Fatalf("ack = %+v", ack)
	}
}

func TestReadOnlyHidesAndRefusesDestructiveTools(t *testing.T) {
	h := newHarness(t, true)
	tools, err := h.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 13 {
		t.Fatalf("%d tools in read-only mode", len(tools.Tools))
	}
	for _, tl := range tools.Tools {
		if a := tl.Annotations; a == nil || !a.ReadOnlyHint {
			t.Fatalf("%s listed in read-only mode", tl.Name)
		}
	}
	for _, name := range append(destructiveTools, writeTools...) {
		res, err := h.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: map[string]any{"space": "rolling-demo", "service": "web", "job": "migrate"}})
		if err == nil && !res.IsError {
			t.Fatalf("%s ran in read-only mode", name)
		}
	}
	if h.node.hit("POST /v1/spaces/rolling-demo/restart/web") || h.node.hit("POST /v1/spaces/rolling-demo/down") {
		t.Fatal("a destructive call reached the node in read-only mode")
	}
}
