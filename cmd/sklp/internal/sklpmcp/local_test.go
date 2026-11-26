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

func TestDevValidateReportsTheFailure(t *testing.T) {
	h := newLocalHarness(t, false)
	var out outputOut
	h.ok("dev_validate", map[string]any{"repo": h.repo, "stack": "web"}, &out)
	if out.OK || out.ExitCode != 1 || !strings.Contains(out.Output[0], "unknown probe") {
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
	want := "deploy_contexts,dev_logs,dev_ls,dev_validate,job_status,space_get,space_ls,stacks"
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
