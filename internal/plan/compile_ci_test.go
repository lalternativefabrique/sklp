package plan

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

// allMatch always matches — useful when the test does not exercise
// the scope path.
func allMatch(_ []string, _ map[string][]string, _ []string) (bool, error) {
	return true, nil
}

// noMatch always fails to match — used to assert StatusSkippedCompile.
func noMatch(_ []string, _ map[string][]string, _ []string) (bool, error) {
	return false, nil
}

// errMatch surfaces an unknown-scope error as the runner does today.
func errMatch(_ []string, _ map[string][]string, _ []string) (bool, error) {
	return false, errors.New("scope \"typo\" is not declared")
}

// TestResolveStepTools covers the three-state contract. With the
// config∪pipeline union supplied as pipelineTools, a nil-tools step
// inherits the FULL effective env (every union key, sorted), an explicit
// list is kept verbatim, and an empty list opts out.
func TestResolveStepTools(t *testing.T) {
	union := map[string]string{"go": "1.25", "python": "3.12"}

	t.Run("nil inherits the full union, sorted", func(t *testing.T) {
		got := resolveStepTools(nil, union)
		want := []string{"go", "python"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("resolveStepTools(nil, union) = %v, want %v", got, want)
		}
	})

	t.Run("explicit list kept verbatim", func(t *testing.T) {
		got := resolveStepTools([]string{"python"}, union)
		if !reflect.DeepEqual(got, []string{"python"}) {
			t.Fatalf("resolveStepTools = %v, want [python]", got)
		}
	})

	t.Run("empty list opts out", func(t *testing.T) {
		if got := resolveStepTools([]string{}, union); got != nil {
			t.Fatalf("resolveStepTools([], union) = %v, want nil", got)
		}
	})
}

func TestCompileCI_HappyPath(t *testing.T) {
	in := CompileCIInput{
		Steps: []CIStepInput{
			{Name: "test", Run: "go test ./...", Tools: []string{"go"}},
			{
				Name: "lint",
				Run:  "pnpm lint",
				Env:  map[string]string{"NODE_ENV": "test"},
			},
		},
		PipelineEnv:   map[string]string{"CI": "true"},
		PipelineTools: map[string]string{"go": "1.25.0", "pnpm": "10.0.0"},
		Secrets:       []string{"GITHUB_TOKEN"},
	}
	pl, err := CompileCI(in, allMatch, nil)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if pl.SchemaVersion != SchemaVersion {
		t.Errorf("schema_version = %d, want %d", pl.SchemaVersion, SchemaVersion)
	}
	if pl.Kind != KindCI {
		t.Errorf("kind = %q", pl.Kind)
	}
	if pl.CI == nil || len(pl.CI.Steps) != 2 {
		t.Fatalf("CI steps not compiled: %+v", pl.CI)
	}

	// First step keeps its explicit tool subset.
	if got := pl.CI.Steps[0].Tools; !reflect.DeepEqual(got, []string{"go"}) {
		t.Errorf("step 0 tools = %v, want [go]", got)
	}

	// First step inherits the pipeline-level env.
	if pl.CI.Steps[0].Env["CI"] != "true" {
		t.Errorf("step 0 env missing CI=true: %+v", pl.CI.Steps[0].Env)
	}

	// Second step layers its env on top.
	if pl.CI.Steps[1].Env["NODE_ENV"] != "test" || pl.CI.Steps[1].Env["CI"] != "true" {
		t.Errorf("step 1 env wrong: %+v", pl.CI.Steps[1].Env)
	}

	// Second step inherits the full pipeline tool list (nil = inherit).
	if got := pl.CI.Steps[1].Tools; len(got) != 2 {
		t.Errorf("step 1 tools = %v, want both pipeline tools", got)
	}

	if !reflect.DeepEqual(pl.CI.Secrets, []string{"GITHUB_TOKEN"}) {
		t.Errorf("secrets not carried: %v", pl.CI.Secrets)
	}
}

func TestCompileCI_ScopeSkip(t *testing.T) {
	in := CompileCIInput{
		Steps: []CIStepInput{
			{Name: "scoped", Run: "echo x", Scope: []string{"backend"}},
		},
		Scopes:       map[string][]string{"backend": {"apps/core/**"}},
		ChangedFiles: []string{"apps/web/main.tsx"},
	}
	pl, err := CompileCI(in, noMatch, nil)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if pl.CI.Steps[0].Status != StatusSkippedCompile {
		t.Errorf("status = %q, want skipped_at_compile", pl.CI.Steps[0].Status)
	}
	if pl.CI.Steps[0].Run != "" {
		t.Errorf("skipped step should have empty Run, got %q", pl.CI.Steps[0].Run)
	}
}

func TestCompileCI_IfRejected(t *testing.T) {
	in := CompileCIInput{
		Steps: []CIStepInput{
			{Name: "gated", Run: "true", If: "changed(\"x\")"},
		},
	}
	_, err := CompileCI(in, allMatch, nil)
	var ce *CICompileError
	if !errors.As(err, &ce) {
		t.Fatalf("want CICompileError, got %v", err)
	}
	if ce.Step != "gated" {
		t.Errorf("step = %q, want gated", ce.Step)
	}
}

func TestCompileCI_EmptyRunRejected(t *testing.T) {
	in := CompileCIInput{Steps: []CIStepInput{{Name: "blank"}}}
	_, err := CompileCI(in, allMatch, nil)
	var ce *CICompileError
	if !errors.As(err, &ce) {
		t.Fatalf("want CICompileError, got %v", err)
	}
}

func TestCompileCI_ScopeMatcherError(t *testing.T) {
	in := CompileCIInput{
		Steps: []CIStepInput{{Name: "oops", Run: "echo x", Scope: []string{"typo"}}},
	}
	_, err := CompileCI(in, errMatch, nil)
	var ce *CICompileError
	if !errors.As(err, &ce) {
		t.Fatalf("want CICompileError, got %v", err)
	}
	if ce.Step != "oops" {
		t.Errorf("step = %q", ce.Step)
	}
}

// TestCompileCI_RoundTripJSON pins the acceptance criterion: a CI Plan
// must survive JSON marshal/unmarshal byte-identically so it can be
// cached without loss.
func TestCompileCI_RoundTripJSON(t *testing.T) {
	in := CompileCIInput{
		Steps: []CIStepInput{
			{Name: "test", Run: "go test ./...", Tools: []string{"go"}, Services: []string{"pg"}},
			{Name: "skipped", Run: "echo no", Scope: []string{"web"}},
		},
		Scopes:        map[string][]string{"web": {"apps/web/**"}},
		ChangedFiles:  []string{"apps/core/main.go"},
		PipelineEnv:   map[string]string{"CI": "true"},
		PipelineTools: map[string]string{"go": "1.25.0"},
		Secrets:       []string{"GH_TOKEN"},
	}
	// Mix matched + skipped: real match for "test", no match for "skipped".
	matcher := func(stepScopes []string, _ map[string][]string, _ []string) (bool, error) {
		for _, s := range stepScopes {
			if s == "web" {
				return false, nil
			}
		}
		return true, nil
	}
	pl, err := CompileCI(in, matcher, nil)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	b, err := json.Marshal(pl)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded Plan
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(*pl, decoded) {
		t.Fatalf("round trip mismatch:\n  pre = %+v\n  post= %+v", *pl, decoded)
	}
}

// TestCompileCI_ImplicitLinearChain pins criterion (3): a pipeline that
// does not declare any depends_on keeps the historical declared order.
func TestCompileCI_ImplicitLinearChain(t *testing.T) {
	in := CompileCIInput{
		Steps: []CIStepInput{
			{Name: "first", Run: "echo 1"},
			{Name: "second", Run: "echo 2"},
			{Name: "third", Run: "echo 3"},
		},
	}
	pl, err := CompileCI(in, allMatch, nil)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	got := []string{
		pl.CI.Steps[0].Name,
		pl.CI.Steps[1].Name,
		pl.CI.Steps[2].Name,
	}
	want := []string{"first", "second", "third"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
	// Linear chain: each step depends on the previous.
	if got := pl.CI.Steps[0].DependsOn; len(got) != 0 {
		t.Errorf("step 0 deps = %v, want empty", got)
	}
	if got := pl.CI.Steps[1].DependsOn; !reflect.DeepEqual(got, []string{"first"}) {
		t.Errorf("step 1 deps = %v", got)
	}
	if got := pl.CI.Steps[2].DependsOn; !reflect.DeepEqual(got, []string{"second"}) {
		t.Errorf("step 2 deps = %v", got)
	}
}

// TestCompileCI_ExplicitDAGOrder checks the deterministic topo order.
func TestCompileCI_ExplicitDAGOrder(t *testing.T) {
	// Diamond: build -> {lint, test} -> deploy.
	in := CompileCIInput{
		Steps: []CIStepInput{
			{Name: "deploy", Run: "echo deploy", DependsOn: []string{"lint", "test"}},
			{Name: "test", Run: "echo test", DependsOn: []string{"build"}},
			{Name: "lint", Run: "echo lint", DependsOn: []string{"build"}},
			{Name: "build", Run: "echo build"},
		},
	}
	pl, err := CompileCI(in, allMatch, nil)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	got := make([]string, len(pl.CI.Steps))
	for i, s := range pl.CI.Steps {
		got[i] = s.Name
	}
	want := []string{"build", "lint", "test", "deploy"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
	// Adjacency must surface build -> {lint, test} for engine 2.5.
	if got := pl.CI.Adjacency["build"]; !reflect.DeepEqual(got, []string{"lint", "test"}) {
		t.Errorf("adjacency[build] = %v", got)
	}
}

// TestCompileCI_CycleRejected pins criterion (1).
func TestCompileCI_CycleRejected(t *testing.T) {
	in := CompileCIInput{
		Steps: []CIStepInput{
			{Name: "a", Run: "echo a", DependsOn: []string{"b"}},
			{Name: "b", Run: "echo b", DependsOn: []string{"a"}},
		},
	}
	_, err := CompileCI(in, allMatch, nil)
	var ce *CICompileError
	if !errors.As(err, &ce) {
		t.Fatalf("want CICompileError, got %v", err)
	}
	if ce.Reason == "" {
		t.Fatalf("empty reason")
	}
	// Message must name both endpoints.
	if !contains(ce.Reason, "a") || !contains(ce.Reason, "b") {
		t.Errorf("reason %q does not name both endpoints", ce.Reason)
	}
}

// TestCompileCI_MissingDepRejected pins criterion (4).
func TestCompileCI_MissingDepRejected(t *testing.T) {
	in := CompileCIInput{
		Steps: []CIStepInput{
			{Name: "a", Run: "echo a", DependsOn: []string{"ghost"}},
		},
	}
	_, err := CompileCI(in, allMatch, nil)
	var ce *CICompileError
	if !errors.As(err, &ce) {
		t.Fatalf("want CICompileError, got %v", err)
	}
	if ce.Step != "a" {
		t.Errorf("step = %q", ce.Step)
	}
	if !contains(ce.Reason, "ghost") {
		t.Errorf("reason missing target name: %q", ce.Reason)
	}
}

// TestCompileCI_ByteIdenticalJSON pins criterion (2): two compiles of
// the same input yield byte-identical CIPlan JSON.
func TestCompileCI_ByteIdenticalJSON(t *testing.T) {
	in := CompileCIInput{
		Steps: []CIStepInput{
			{Name: "deploy", Run: "echo deploy", DependsOn: []string{"lint", "test"}},
			{Name: "test", Run: "echo test", DependsOn: []string{"build"}},
			{Name: "lint", Run: "echo lint", DependsOn: []string{"build"}},
			{Name: "build", Run: "echo build"},
		},
		PipelineEnv:   map[string]string{"CI": "true", "FOO": "bar"},
		PipelineTools: map[string]string{"go": "1.25.0", "node": "22"},
		Secrets:       []string{"GITHUB_TOKEN"},
	}
	pl1, err := CompileCI(in, allMatch, nil)
	if err != nil {
		t.Fatalf("compile1: %v", err)
	}
	pl2, err := CompileCI(in, allMatch, nil)
	if err != nil {
		t.Fatalf("compile2: %v", err)
	}
	b1, err := json.Marshal(pl1)
	if err != nil {
		t.Fatalf("marshal1: %v", err)
	}
	b2, err := json.Marshal(pl2)
	if err != nil {
		t.Fatalf("marshal2: %v", err)
	}
	if string(b1) != string(b2) {
		t.Fatalf("non-deterministic JSON:\n  a = %s\n  b = %s", b1, b2)
	}
}

// TestCompileCI_DuplicateStepRejected guards uniqueness — the DAG would
// otherwise be ambiguous.
func TestCompileCI_DuplicateStepRejected(t *testing.T) {
	in := CompileCIInput{
		Steps: []CIStepInput{
			{Name: "a", Run: "echo a"},
			{Name: "a", Run: "echo a2"},
		},
	}
	_, err := CompileCI(in, allMatch, nil)
	var ce *CICompileError
	if !errors.As(err, &ce) {
		t.Fatalf("want CICompileError, got %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestCompileCI_RejectsInputPathEscape(t *testing.T) {
	in := CompileCIInput{
		Steps: []CIStepInput{
			{Name: "bad", Run: "true", Inputs: []string{"../../etc/passwd"}},
		},
	}
	_, err := CompileCI(in, allMatch, nil)
	if err == nil {
		t.Fatalf("expected compile error for escaping inputs path")
	}
	var ce *CICompileError
	if !errors.As(err, &ce) || ce.Step != "bad" {
		t.Fatalf("wrong error shape: %v", err)
	}
}

func TestCompileCI_RejectsOutputAbsolutePath(t *testing.T) {
	in := CompileCIInput{
		Steps: []CIStepInput{
			{Name: "bad", Run: "true", Outputs: []string{"/tmp/foo"}},
		},
	}
	_, err := CompileCI(in, allMatch, nil)
	if err == nil {
		t.Fatalf("expected compile error for absolute output path")
	}
}

func TestCompileCI_InputsAreResolved(t *testing.T) {
	in := CompileCIInput{
		Steps: []CIStepInput{
			{Name: "with-inputs", Run: "true", Inputs: []string{"src/**/*.go"}},
		},
	}
	resolver := func(globs []string) ([]string, error) {
		return []string{"src/a.go", "src/b.go"}, nil
	}
	pl, err := CompileCI(in, allMatch, resolver)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	got := pl.CI.Steps[0].ResolvedInputs
	if len(got) != 2 || got[0] != "src/a.go" || got[1] != "src/b.go" {
		t.Fatalf("resolved inputs not stored: %v", got)
	}
}
