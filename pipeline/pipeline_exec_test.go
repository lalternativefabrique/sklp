package pipeline

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runPipelineCase loads a YAML, runs it, and returns everything a
// test might want to assert on. Centralised so the test file stays
// short and readable.
type runPipelineCase struct {
	yaml         string
	envOverrides map[string]string
	// files written under RepoPath before the run (relative path -> content).
	// Lets a test stage a `.env.local` so env_files resolution + secret
	// resolution are exercised the way a real local pipeline run is.
	repoFiles map[string]string
	// setup runs against RepoPath before the run, for fixtures a flat
	// path->content map cannot express (an encrypted .sklp/config.yaml).
	setup func(t *testing.T, repo string)
}

func runPipelineFromYAML(t *testing.T, c runPipelineCase) (PipelineResult, string, string, error) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ci.yaml")
	if err := os.WriteFile(path, []byte(c.yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	for rel, content := range c.repoFiles {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o600); err != nil {
			t.Fatalf("write repo file %s: %v", rel, err)
		}
	}
	if c.setup != nil {
		c.setup(t, dir)
	}

	// Apply env overrides for the duration of the test so secret
	// resolution sees them. `t.Setenv` rolls them back on teardown.
	for k, v := range c.envOverrides {
		t.Setenv(k, v)
	}

	p, err := LoadPipeline(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	var stdout, stderr bytes.Buffer
	res, runErr := RunPipeline(context.Background(), p, PipelineRunOptions{
		RepoPath: dir,
		Stdout:   &stdout,
		Stderr:   &stderr,
	})
	return res, stdout.String(), stderr.String(), runErr
}

func TestRunPipeline_HappyPath(t *testing.T) {
	res, stdout, _, err := runPipelineFromYAML(t, runPipelineCase{
		yaml: `
version: 1
kind: pipeline
steps:
  - name: hello
    run: echo hello-world
  - name: math
    run: |
      result=$((2 + 2))
      echo result=$result
`,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.PassFail != "pass" {
		t.Errorf("pass_fail = %q, want pass", res.PassFail)
	}
	if len(res.Steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(res.Steps))
	}
	for _, s := range res.Steps {
		if s.Status != "passed" {
			t.Errorf("step %q status = %q, want passed", s.Name, s.Status)
		}
	}
	if !strings.Contains(stdout, "hello-world") {
		t.Errorf("missing step output: %q", stdout)
	}
	if !strings.Contains(stdout, "result=4") {
		t.Errorf("missing multi-line output: %q", stdout)
	}
}

func TestRunPipeline_FailureStopsRun(t *testing.T) {
	res, _, _, err := runPipelineFromYAML(t, runPipelineCase{
		yaml: `
version: 1
kind: pipeline
steps:
  - name: ok
    run: "true"
  - name: boom
    run: "exit 17"
  - name: never
    run: "echo never-runs"
`,
	})
	if !errors.Is(err, ErrPipelineFailed) {
		t.Fatalf("expected ErrPipelineFailed, got %v", err)
	}
	if res.PassFail != "fail" {
		t.Errorf("pass_fail = %q, want fail", res.PassFail)
	}
	want := []string{"passed", "failed", "skipped"}
	for i, w := range want {
		if res.Steps[i].Status != w {
			t.Errorf("step[%d] status = %q, want %q", i, res.Steps[i].Status, w)
		}
	}
	if res.Steps[1].ExitCode != 17 {
		t.Errorf("step[1] exit code = %d, want 17", res.Steps[1].ExitCode)
	}
}

func TestRunPipeline_EnvAndStepEnv(t *testing.T) {
	_, stdout, _, err := runPipelineFromYAML(t, runPipelineCase{
		yaml: `
version: 1
kind: pipeline
env:
  FROM_PIPELINE: pipe
  COMMON: pipeline-wins
steps:
  - name: echo-vars
    env:
      FROM_STEP: step
      COMMON: step-wins
    run: |
      echo "pipeline=$FROM_PIPELINE step=$FROM_STEP common=$COMMON"
`,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(stdout, "pipeline=pipe step=step common=step-wins") {
		t.Errorf("env merge wrong: %q", stdout)
	}
}

func TestRunPipeline_SecretMissingFailsEarly(t *testing.T) {
	res, stdout, _, err := runPipelineFromYAML(t, runPipelineCase{
		yaml: `
version: 1
kind: pipeline
secrets:
  - SKLP_TEST_REQUIRED_SECRET
steps:
  - name: never
    run: "echo should-not-run"
`,
	})
	if err == nil {
		t.Fatal("expected error for missing secret")
	}
	if !strings.Contains(err.Error(), "SKLP_TEST_REQUIRED_SECRET") {
		t.Errorf("error missing secret name: %v", err)
	}
	if strings.Contains(stdout, "should-not-run") {
		t.Errorf("step ran despite missing secret")
	}
	// No step should be marked as anything; we bail before the loop.
	if len(res.Steps) != 0 {
		t.Errorf("expected 0 step results, got %+v", res.Steps)
	}
}

func TestRunPipeline_SecretPresentFlowsToStep(t *testing.T) {
	_, stdout, _, err := runPipelineFromYAML(t, runPipelineCase{
		yaml: `
version: 1
kind: pipeline
secrets:
  - SKLP_TEST_SECRET
steps:
  - name: show
    secrets: [SKLP_TEST_SECRET]
    run: 'echo got=$SKLP_TEST_SECRET'
`,
		envOverrides: map[string]string{
			"SKLP_TEST_SECRET": "shhh",
		},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(stdout, "got=shhh") {
		t.Errorf("secret not propagated: %q", stdout)
	}
}

// A secret provided ONLY via .env.local (absent from the shell env) must be
// resolved and reach a step that opts into it. This is the canonical "give the
// local CI a secret" path: env_files load (os.Setenv) happens before secret
// resolution, so the dotenv value is visible to resolveSecrets and the step.
func TestRunPipeline_SecretFromDotenvReachesStep(t *testing.T) {
	// Guarantee the host env is not the source — only .env.local provides it.
	os.Unsetenv("SKLP_TEST_DOTENV_SECRET")
	_, stdout, _, err := runPipelineFromYAML(t, runPipelineCase{
		yaml: `
version: 1
kind: pipeline
env_files:
  - .env.local
secrets:
  - SKLP_TEST_DOTENV_SECRET
steps:
  - name: show
    secrets: [SKLP_TEST_DOTENV_SECRET]
    run: 'echo got=$SKLP_TEST_DOTENV_SECRET'
`,
		repoFiles: map[string]string{
			".env.local": "SKLP_TEST_DOTENV_SECRET=from-dotenv\n",
		},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(stdout, "got=from-dotenv") {
		t.Errorf("dotenv secret not propagated to step: %q", stdout)
	}
}

func TestRunPipeline_IfRejectedV1(t *testing.T) {
	_, _, _, err := runPipelineFromYAML(t, runPipelineCase{
		yaml: `
version: 1
kind: pipeline
steps:
  - name: gated
    if: 'changed("apps/web/**")'
    run: "true"
`,
	})
	if err == nil {
		t.Fatal("expected error: `if:` not implemented yet")
	}
	if !strings.Contains(err.Error(), "if:") {
		t.Errorf("error should mention the if: field, got %v", err)
	}
}

// (Test for an unknown tool's failure removed when the skt fallback
// landed: an unknown tool now routes through skt, covered by
// internal/toolchain without touching the runner package.)

func TestIsPipelineKind(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"pipeline lowercase", "kind: pipeline\n", true},
		{"pipeline mixed case", "kind: Pipeline\n", true},
		{"legacy task", "kind: task\n", false},
		{"missing kind", "name: foo\n", false},
		{"bogus yaml", ":::not yaml", false},
	}
	for _, c := range cases {
		if got := isPipelineKind([]byte(c.in)); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// resolveSecrets must strip a matched pair of surrounding quotes so a
// host-exported secret like S3_SECRET_KEY='"abc"' reaches the tool as the
// bare value, not with quotes baked in (which broke SigV4 signing).
func TestResolveSecretsStripsSurroundingQuotes(t *testing.T) {
	cases := []struct {
		name, set, want string
	}{
		{"bare", "abc123", "abc123"},
		{"double-quoted", `"abc123"`, "abc123"},
		{"single-quoted", `'abc123'`, "abc123"},
		{"unmatched-leading", `"abc123`, `"abc123`},
		{"inner-quote-kept", `ab"c123`, `ab"c123`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("SKLP_TEST_SECRET_Q", c.set)
			out, err := resolveSecrets([]string{"SKLP_TEST_SECRET_Q"}, t.TempDir(), nil)
			if err != nil {
				t.Fatalf("resolveSecrets: %v", err)
			}
			if got := out["SKLP_TEST_SECRET_Q"]; got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
