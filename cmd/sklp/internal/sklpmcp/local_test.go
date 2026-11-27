package sklpmcp

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const fakeEnv = "SKLPMCP_FAKE_SKLP"

func TestMain(m *testing.M) {
	if os.Getenv(fakeEnv) != "" {
		os.Exit(fakeSklp(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeSklp stands in for the sklp binary: it records its argv and answers
// like the verb it was asked for.
func fakeSklp(args []string) int {
	if f, err := os.OpenFile(os.Getenv(fakeEnv), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		wd, _ := os.Getwd()
		fmt.Fprintf(f, "%s|%s\n", wd, strings.Join(args, " "))
		f.Close()
	}
	verb := strings.Join(args[:min(2, len(args))], " ")
	switch {
	case verb == "space ls":
		fmt.Println(`[{"name":"ci-1","status":"running","supervised":false,"containers":[{"id":"c","status":"running"}]}]`)
	case verb == "space get":
		fmt.Printf(`{"name":%q,"status":"stopped","supervised":false,"containers":[]}`+"\n", args[2])
	case verb == "space exec":
		fmt.Println("hello from", strings.Join(args[len(args)-2:], " "))
		fmt.Fprintln(os.Stderr, "token", os.Getenv("SKLPMCP_LEAK"))
		return 3
	case args[0] == "skills":
		fmt.Printf("# guide %s for %s\n", args[len(args)-1], args[2])
	case verb == "dev ls":
		fmt.Println(os.Getenv("SKLPMCP_DEV_LS"))
	case verb == "dev logs":
		for i := range 300 {
			fmt.Println("web | line", i)
		}
	case args[0] == "dev" && len(args) > 2 && args[2] == "-validate":
		fmt.Println("dev.yaml: services.web.ready: unknown probe")
		return 1
	case args[0] == "dev" && len(args) > 2 && args[2] == "-d":
		fmt.Println("✓ stack ready")
	case args[0] == "run":
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT)
		fmt.Println("step vet: ok")
		select {
		case <-sig:
			fmt.Println("interrupted, tearing down")
			return 130
		case <-time.After(time.Duration(len(os.Getenv("SKLPMCP_SLOW"))) * time.Second):
		}
		fmt.Println("step test: ok")
	}
	return 0
}

type localHarness struct {
	*harness
	calls string
	repo  string
}

func newLocalHarness(t *testing.T, readOnly bool) *localHarness {
	t.Helper()
	f := newFakeNode(t)
	writeConfig(t, f)
	calls := filepath.Join(t.TempDir(), "calls")
	t.Setenv(fakeEnv, calls)
	t.Setenv("SKLPMCP_LEAK", testToken)
	t.Setenv("SKLPMCP_DEV_LS", "[]")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	repo := writeRepo(t)
	if err := os.MkdirAll(filepath.Join(repo, ".sklp", "pipelines"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(repo, ".sklp", "pipelines", "ci.yaml"), []byte("version: 1\nkind: pipeline\nname: ci\nsteps: []\n"), 0o644)
	_ = os.WriteFile(filepath.Join(repo, ".sklp", "web", "dev.yaml"), []byte("services: {}\n"), 0o644)
	cs := connect(t, Options{ReadOnly: readOnly, Exe: exe})
	return &localHarness{harness: &harness{t: t, node: f, cs: cs}, calls: calls, repo: repo}
}

func (h *localHarness) argv() []string {
	data, _ := os.ReadFile(h.calls)
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func (h *localHarness) waitJob(id string) jobOut {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var out jobsOut
		h.ok("job_status", map[string]any{"id": id}, &out)
		if out.Jobs[0].Status != "running" {
			return out.Jobs[0]
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.t.Fatalf("job %s never finished", id)
	return jobOut{}
}

func TestStacksListsContractsAndPipelines(t *testing.T) {
	h := newLocalHarness(t, false)
	var out stacksOut
	h.ok("stacks", map[string]any{"repo": h.repo}, &out)
	if len(out.Stacks) != 1 || out.Stacks[0].Name != "web" || strings.Join(out.Stacks[0].Contracts, ",") != "deploy,dev" || strings.Join(out.Pipelines, ",") != "ci" {
		t.Fatalf("got %+v", out)
	}
}

func TestSpaceLsGetAndExec(t *testing.T) {
	h := newLocalHarness(t, false)
	var spaces localSpacesOut
	h.ok("space_ls", nil, &spaces)
	if len(spaces.Spaces) != 1 || spaces.Spaces[0].Name != "ci-1" || len(spaces.Spaces[0].Containers) != 1 {
		t.Fatalf("got %+v", spaces)
	}
	var one localSpace
	h.ok("space_get", map[string]any{"name": "ci-1"}, &one)
	if one.Name != "ci-1" || one.Status != "stopped" {
		t.Fatalf("got %+v", one)
	}
	if res, _ := h.call("space_get", map[string]any{"name": "-rf"}); !res.IsError {
		t.Fatal("a flag-like space name must be refused")
	}
	var out outputOut
	h.ok("space_exec", map[string]any{"name": "ci-1", "container": "core", "command": []string{"go", "test"}}, &out)
	if out.OK || out.ExitCode != 3 || !strings.Contains(strings.Join(out.Output, "\n"), "hello from go test") {
		t.Fatalf("got %+v", out)
	}
	if got := h.argv(); !strings.HasSuffix(got[len(got)-1], "|space exec ci-1 -c core -- go test") {
		t.Fatalf("argv = %v", got)
	}
}

func TestValidateDevReportsTheLoaderFailure(t *testing.T) {
	h := newLocalHarness(t, false)
	var out validateOut
	h.ok("contract_validate", map[string]any{"repo": h.repo, "contract": "dev", "stack": "web"}, &out)
	if out.Valid || len(out.Errors) == 0 || !strings.Contains(out.Errors[0], "unknown probe") {
		t.Fatalf("got %+v", out)
	}
}

func TestDevUpRunsInTheBackgroundAndOnlyOneStackAtATime(t *testing.T) {
	h := newLocalHarness(t, false)
	var started jobOut
	h.ok("dev_up", map[string]any{"repo": h.repo, "stack": "web", "only": []string{"api", "db"}}, &started)
	done := h.waitJob(started.ID)
	if done.Status != "succeeded" || done.Output[0] != "✓ stack ready" {
		t.Fatalf("job = %+v", done)
	}
	argv := h.argv()
	if want := h.repo + "|dev web -d -no-color -repo " + h.repo + " -only api,db"; argv[len(argv)-1] != want {
		t.Fatalf("argv = %q, want %q", argv[len(argv)-1], want)
	}

	t.Setenv("SKLPMCP_DEV_LS", `[{"repo":"/elsewhere","stack":"core","running":true}]`)
	res, text := h.call("dev_up", map[string]any{"repo": h.repo, "stack": "web"})
	if !res.IsError || !strings.Contains(text, "one dev stack at a time") {
		t.Fatalf("got %v %q", res.IsError, text)
	}
	var ls devLsOut
	h.ok("dev_ls", nil, &ls)
	if len(ls.Stacks) != 1 || !ls.Stacks[0].Running {
		t.Fatalf("got %+v", ls)
	}
}

func TestDevLogsIsBounded(t *testing.T) {
	h := newLocalHarness(t, false)
	var out linesOut
	h.ok("dev_logs", map[string]any{"repo": h.repo, "stack": "web", "lines": 5}, &out)
	if len(out.Lines) != 5 || out.Lines[4] != "web | line 299" {
		t.Fatalf("got %+v", out)
	}
}

func TestRunPipelineIsAJobThatCanBeCancelled(t *testing.T) {
	h := newLocalHarness(t, false)
	if res, text := h.call("run_pipeline", map[string]any{"repo": h.repo, "pipeline": "nope"}); !res.IsError || !strings.Contains(text, "have: ci") {
		t.Fatalf("got %v %q", res.IsError, text)
	}
	var job jobOut
	h.ok("run_pipeline", map[string]any{"repo": h.repo, "pipeline": "ci"}, &job)
	if done := h.waitJob(job.ID); done.Status != "succeeded" || strings.Join(done.Output, ";") != "step vet: ok;step test: ok" {
		t.Fatalf("job = %+v", done)
	}

	t.Setenv("SKLPMCP_SLOW", strings.Repeat("x", 30))
	h.ok("run_pipeline", map[string]any{"repo": h.repo, "pipeline": "ci"}, &job)
	if res, text := h.call("run_pipeline", map[string]any{"repo": h.repo, "pipeline": "ci"}); !res.IsError || !strings.Contains(text, "still running") {
		t.Fatalf("a second run must wait for the first: %v %q", res.IsError, text)
	}
	for !strings.Contains(strings.Join(h.waitOutput(job.ID), ";"), "step vet") {
		time.Sleep(20 * time.Millisecond)
	}
	var cancelled jobOut
	h.ok("job_cancel", map[string]any{"id": job.ID}, &cancelled)
	if cancelled.Status != "failed" || *cancelled.ExitCode != 130 || !strings.Contains(strings.Join(cancelled.Output, ";"), "tearing down") {
		t.Fatalf("cancel = %+v", cancelled)
	}
	var all jobsOut
	h.ok("job_status", nil, &all)
	if len(all.Jobs) != 2 || all.Jobs[0].ID != job.ID {
		t.Fatalf("jobs = %+v", all)
	}
}

func (h *localHarness) waitOutput(id string) []string {
	var out jobsOut
	h.ok("job_status", map[string]any{"id": id}, &out)
	return out.Jobs[0].Output
}

func TestReadOnlyServesOnlyLocalReads(t *testing.T) {
	h := newLocalHarness(t, true)
	tools, err := h.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
	}
	want := "contract_validate,deploy_contexts,dev_logs,dev_ls,job_status,sklp_draft,sklp_guide,sklp_schema,space_get,space_ls,stacks"
	if got := strings.Join(slices.Sorted(slices.Values(names)), ","); got != want {
		t.Fatalf("read-only tools = %s, want %s", got, want)
	}
	res, err := h.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "space_down", Arguments: map[string]any{"name": "ci-1"}})
	if err == nil && !res.IsError {
		t.Fatal("space_down ran in read-only mode")
	}
	if data, _ := os.ReadFile(h.calls); strings.Contains(string(data), "space down") {
		t.Fatal("space down reached sklp in read-only mode")
	}
}

func TestGuideIsTheRepoTailoredSkill(t *testing.T) {
	h := newLocalHarness(t, false)
	var out guideOut
	h.ok("sklp_guide", map[string]any{"repo": h.repo, "contract": "space"}, &out)
	if out.Guide != "# guide init-config for "+h.repo+"\n" {
		t.Fatalf("guide = %q", out.Guide)
	}
	if res, text := h.call("sklp_guide", map[string]any{"repo": h.repo, "contract": "nope"}); !res.IsError || !strings.Contains(text, "ci, deploy, dev, operator, publish, space, watches") {
		t.Fatalf("unknown contract: %v %q", res.IsError, text)
	}
}

func TestDraftScansTheRepoAndWritesNothing(t *testing.T) {
	h := newLocalHarness(t, false)
	_ = os.WriteFile(filepath.Join(h.repo, "go.mod"), []byte("module example.com/app\n\ngo 1.26\n"), 0o644)
	var ci draftOut
	h.ok("sklp_draft", map[string]any{"repo": h.repo, "contract": "ci"}, &ci)
	if ci.Path != ".sklp/pipelines/ci.yaml" || !ci.Exists || !strings.Contains(ci.YAML, "kind: pipeline") {
		t.Fatalf("ci draft = %+v", ci)
	}
	var dev draftOut
	h.ok("sklp_draft", map[string]any{"repo": h.repo, "contract": "dev", "stack": "web"}, &dev)
	if dev.Path != ".sklp/web/dev.yaml" || !dev.Exists {
		t.Fatalf("dev draft = %+v", dev)
	}
	if data, _ := os.ReadFile(filepath.Join(h.repo, ".sklp", "pipelines", "ci.yaml")); strings.Contains(string(data), "go test") {
		t.Fatal("sklp_draft wrote the file")
	}
	if res, _ := h.call("sklp_draft", map[string]any{"repo": h.repo, "contract": "dev", "stack": "../x"}); !res.IsError {
		t.Fatal("a path-like stack must be refused")
	}
}

func TestSchemaServesEachContract(t *testing.T) {
	h := newLocalHarness(t, false)
	for contract, suffix := range map[string]string{
		"pipeline":      "/pipeline/pipeline.schema.json",
		"deploy":        "/schema/deploy.schema.json",
		"deploy-root":   "/schema/deploy-root.schema.json",
		"image-watches": "/schema/image-watches.schema.json",
	} {
		var out struct {
			Schema struct {
				ID string `json:"$id"`
			} `json:"schema"`
		}
		h.ok("sklp_schema", map[string]any{"contract": contract}, &out)
		if !strings.HasSuffix(out.Schema.ID, suffix) {
			t.Fatalf("%s schema id = %q", contract, out.Schema.ID)
		}
	}
}

func TestValidatePipeline(t *testing.T) {
	h := newLocalHarness(t, false)
	dir := filepath.Join(h.repo, ".sklp", "pipelines")
	_ = os.WriteFile(filepath.Join(dir, "ci.yaml"), []byte("version: 1\nkind: pipeline\nname: ci\nsteps:\n  - name: vet\n    run: go vet ./...\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte("version: 1\nkind: pipeline\nname: bad\nsteps:\n  - name: t\n    services: [ghost]\n    run: echo\n"), 0o644)
	var ok validateOut
	h.ok("contract_validate", map[string]any{"repo": h.repo, "contract": "pipeline", "pipeline": "ci"}, &ok)
	if !ok.Valid || strings.Join(ok.Steps, ",") != "vet" {
		t.Fatalf("ci = %+v", ok)
	}
	var bad validateOut
	h.ok("contract_validate", map[string]any{"repo": h.repo, "contract": "pipeline", "pipeline": "bad"}, &bad)
	if bad.Valid || len(bad.Errors) == 0 {
		t.Fatalf("bad = %+v", bad)
	}
}

func TestValidateDeploySeparatesErrorsFromIgnoredKeys(t *testing.T) {
	h := newLocalHarness(t, false)
	var ok validateOut
	h.ok("contract_validate", map[string]any{"repo": h.repo, "contract": "deploy", "stack": "web"}, &ok)
	if !ok.Valid || len(ok.Warnings) != 0 {
		t.Fatalf("fixture = %+v", ok)
	}
	bad := "version: 1\nkind: deploy\nservices:\n  web:\n    image: x\n    port: ${PORT}\n    limits: {memory: 1G}\n    depends_on: [db]\n  api:\n    port: eighty\n"
	_ = os.WriteFile(filepath.Join(h.repo, ".sklp", "web", "deploy.yaml"), []byte(bad), 0o644)
	var out validateOut
	h.ok("contract_validate", map[string]any{"repo": h.repo, "contract": "deploy", "stack": "web"}, &out)
	all := strings.Join(out.Errors, "\n")
	if out.Valid || !strings.Contains(all, `depends_on "db"`) || !strings.Contains(all, "services.api: image is required") || !strings.Contains(all, "/services/api/port") {
		t.Fatalf("errors = %q", out.Errors)
	}
	if strings.Contains(all, "/services/web/port") {
		t.Fatalf("a ${VAR} value was flagged: %q", out.Errors)
	}
	if len(out.Warnings) != 1 || !strings.Contains(out.Warnings[0], `"limits"`) {
		t.Fatalf("warnings = %q", out.Warnings)
	}
}

func TestValidateDeployRootAndWatchesAcrossFiles(t *testing.T) {
	h := newLocalHarness(t, false)
	sk := filepath.Join(h.repo, ".sklp")
	_ = os.WriteFile(filepath.Join(sk, "deploy-root.yaml"), []byte("kind: deploy-root\nnode: demo\ndeployments:\n  - space: prod\n    path: .sklp/web/deploy.yaml\n  - space: gone\n    path: .sklp/nope/deploy.yaml\n"), 0o644)
	var root validateOut
	h.ok("contract_validate", map[string]any{"repo": h.repo, "contract": "deploy-root"}, &root)
	if root.Valid || len(root.Errors) != 1 || !strings.Contains(root.Errors[0], ".sklp/nope/deploy.yaml does not exist") {
		t.Fatalf("root = %+v", root)
	}
	watches := "watches:\n  - name: web\n    service: web\n    image_repo: rg.example/ns/web\n    node: demo\n    space: prod\n" +
		"  - name: api\n    service: api\n    image_repo: rg.example/ns/api:v1\n    node: demo\n    space: prod\n    tag_patern: x\n"
	_ = os.WriteFile(filepath.Join(sk, "image-watches.yaml"), []byte(watches), 0o644)
	var w validateOut
	h.ok("contract_validate", map[string]any{"repo": h.repo, "contract": "image-watches"}, &w)
	all := strings.Join(w.Errors, "\n")
	if w.Valid || !strings.Contains(all, "must not carry a tag") || !strings.Contains(all, `service "api" is not in .sklp/web/deploy.yaml`) || strings.Contains(all, "watches[0]") {
		t.Fatalf("watches errors = %q", w.Errors)
	}
	if len(w.Warnings) != 1 || !strings.Contains(w.Warnings[0], "tag_patern") {
		t.Fatalf("watches warnings = %q", w.Warnings)
	}
}

func TestGuidesAreAlsoPrompts(t *testing.T) {
	h := newLocalHarness(t, true)
	ctx := context.Background()
	list, err := h.cs.ListPrompts(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range list.Prompts {
		names = append(names, p.Name)
	}
	if got := strings.Join(slices.Sorted(slices.Values(names)), ","); got != "init-ci,init-config,init-deploy,init-dev,init-publish,init-watches,sklp-operator" {
		t.Fatalf("prompts = %s", got)
	}
	res, err := h.cs.GetPrompt(ctx, &mcp.GetPromptParams{Name: "init-deploy", Arguments: map[string]string{"repo": h.repo}})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Messages[0].Content.(*mcp.TextContent).Text
	if !strings.Contains(text, "# guide init-deploy for "+h.repo) || !strings.Contains(text, "contract_validate") {
		t.Fatalf("prompt = %q", text)
	}
}
