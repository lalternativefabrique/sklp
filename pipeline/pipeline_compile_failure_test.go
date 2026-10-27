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

// writePipelineTask writes a pipeline yaml under repo/.sklp/tasks/test.yaml
// and returns the absolute path. The caller is responsible for repo init.
func writePipelineTask(t *testing.T, repo, yaml string) string {
	t.Helper()
	taskDir := filepath.Join(repo, ".sklp", "tasks")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatalf("mkdir tasks: %v", err)
	}
	taskPath := filepath.Join(taskDir, "test.yaml")
	if err := os.WriteFile(taskPath, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write task: %v", err)
	}
	return taskPath
}

// runText calls Run() in text mode and returns the captured stderr.
func runText(t *testing.T, dir, taskPath string) (string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	_, err := Run(context.Background(), RunConfig{
		TaskPath: taskPath,
		RepoPath: dir,
		NoPull:   true,
		Stdout:   &stdout,
		Stderr:   &stderr,
	})
	return stderr.String(), err
}

// runJSON calls Run() in JSON mode and returns captured stdout.
func runJSON(t *testing.T, dir, taskPath string) (string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	_, err := Run(context.Background(), RunConfig{
		TaskPath:   taskPath,
		RepoPath:   dir,
		NoPull:     true,
		JSONOutput: true,
		Stdout:     &stdout,
		Stderr:     &stderr,
	})
	return stdout.String(), err
}

// TestRun_MissingRegistryCredentialBanner is the headline #439 regression:
// before the fix this scenario exited 1 with no stderr output. A push with no
// stored credential must now surface the CI FAILED banner naming the login
// verb, before any step runs.
func TestRun_MissingRegistryCredentialBanner(t *testing.T) {
	dir := initRepo(t, "main")
	yaml := `version: 1
kind: pipeline
name: test
steps:
  - name: image
    kind: publish
    image: rg.fr-par.scw.cloud/skalpai/core:1
`
	taskPath := writePipelineTask(t, dir, yaml)
	// Isolate from the developer's own registries file so the missing-
	// credential path actually fires rather than passing for the wrong reason.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	got, err := runText(t, dir, taskPath)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var regErr *MissingCredentialError
	if !errors.As(err, &regErr) {
		t.Fatalf("expected MissingCredentialError, got: %v", err)
	}
	for _, want := range []string{
		"CI FAILED",
		"FAILURES",
		"pipeline could not compile",
		`registry: no credential for "rg.fr-par.scw.cloud"`,
		"sklp registry login rg.fr-par.scw.cloud --provider scaleway",
		"hint: run the `sklp registry login` above, then re-run",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("banner missing %q:\n%s", want, got)
		}
	}
}

func TestRun_MissingRegistryCredentialJSON(t *testing.T) {
	dir := initRepo(t, "main")
	yaml := `version: 1
kind: pipeline
name: test
steps:
  - name: image
    kind: publish
    image: rg.fr-par.scw.cloud/skalpai/core:1
`
	taskPath := writePipelineTask(t, dir, yaml)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	out, err := runJSON(t, dir, taskPath)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	for _, want := range []string{
		`"kind":"compile_error"`,
		`"pass_fail":"fail"`,
		`"category":"registry_credential_missing"`,
		`"hosts":"rg.fr-par.scw.cloud"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q\nfull stdout:\n%s", want, out)
		}
	}
}

// TestRun_BuildKitSecretEnvMissingBanner covers `secrets: [{env: X}]`
// on a docker-build step with X unset.
func TestRun_BuildKitSecretEnvMissingBanner(t *testing.T) {
	dir := initRepo(t, "main")
	yaml := `version: 1
kind: pipeline
name: test
steps:
  - name: build
    kind: publish
    image: foo/bar
    tag: latest
    context: .
    push: false
    secrets:
      - id: gh
        env: GITHUB_TOKEN_NOT_SET_439
`
	taskPath := writePipelineTask(t, dir, yaml)
	os.Unsetenv("GITHUB_TOKEN_NOT_SET_439")

	got, err := runText(t, dir, taskPath)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var bkErr *BuildKitSecretEnvMissingError
	if !errors.As(err, &bkErr) {
		t.Fatalf("expected BuildKitSecretEnvMissingError, got: %v", err)
	}
	for _, want := range []string{
		"CI FAILED",
		"pipeline could not compile",
		`secret "gh" requires env var "GITHUB_TOKEN_NOT_SET_439"`,
		"hint: set GITHUB_TOKEN_NOT_SET_439 in your shell or in .env / .env.<pipeline>, then re-run",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stderr missing %q\nfull stderr:\n%s", want, got)
		}
	}
}

// TestRun_TagConfigBothBanner covers `tag:` with both strategy and
// template set — the most common compile-time tag misconfig.
func TestRun_TagConfigBothBanner(t *testing.T) {
	dir := initRepo(t, "main")
	yaml := `version: 1
kind: pipeline
name: test
tag:
  strategy: sha
  template: "v{{ short_sha }}"
steps:
  - name: hello
    run: "true"
`
	taskPath := writePipelineTask(t, dir, yaml)

	got, err := runText(t, dir, taskPath)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var tagErr *TagConfigError
	if !errors.As(err, &tagErr) {
		t.Fatalf("expected TagConfigError, got: %v", err)
	}
	for _, want := range []string{
		"CI FAILED",
		"tag: cannot set both strategy and template",
		"hint: edit `tag:` to set exactly one of `strategy` or `template`",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stderr missing %q\nfull stderr:\n%s", want, got)
		}
	}
}

// TestRun_TagConfigNeitherBanner covers `tag:` with neither field set.
func TestRun_TagConfigNeitherBanner(t *testing.T) {
	dir := initRepo(t, "main")
	yaml := `version: 1
kind: pipeline
name: test
tag: {}
steps:
  - name: hello
    run: "true"
`
	taskPath := writePipelineTask(t, dir, yaml)

	got, err := runText(t, dir, taskPath)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(got, "tag: must set either strategy or template") {
		t.Errorf("stderr missing tag-neither message:\n%s", got)
	}
	if !strings.Contains(got, "hint: edit `tag:`") {
		t.Errorf("stderr missing tag hint:\n%s", got)
	}
}

// TestRun_UnknownScopeBanner covers a step referencing a scope that is
// not declared in `scopes:`. The error is wrapped in a CICompileError
// with no special hint, but the banner + bare message must still
// surface (not silent exit).
func TestRun_UnknownScopeBanner(t *testing.T) {
	dir := initRepo(t, "main")
	yaml := `version: 1
kind: pipeline
name: test
scopes:
  api: ["api/**"]
steps:
  - name: hello
    scope: [does-not-exist]
    run: "true"
`
	taskPath := writePipelineTask(t, dir, yaml)

	got, err := runText(t, dir, taskPath)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	for _, want := range []string{
		"CI FAILED",
		"pipeline could not compile",
		`scope "does-not-exist" referenced by a step is not declared in ` + "`scopes:`",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stderr missing %q\nfull stderr:\n%s", want, got)
		}
	}
}

// TestRun_RequireViolationBannerPreserved is the PR #424 regression
// guard: the bespoke require: banner must still render with its full
// porcelain stanza and remediation hint.
func TestRun_RequireViolationBannerPreserved(t *testing.T) {
	dir := initRepo(t, "main")
	if err := os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write dirty: %v", err)
	}
	yaml := `version: 1
kind: pipeline
name: test
require:
  clean_tree: true
steps:
  - name: hello
    run: "true"
`
	taskPath := writePipelineTask(t, dir, yaml)

	got, err := runText(t, dir, taskPath)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrRequireViolation) {
		t.Fatalf("expected ErrRequireViolation, got: %v", err)
	}
	// The require banner has its own format (no generic "CI FAILED"):
	// keep it byte-stable to avoid breaking the #424 contract.
	for _, want := range []string{
		"refusing to run: working tree dirty",
		"?? dirty.txt",
		"require.clean_tree: true",
		"Commit or stash, then re-run",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stderr missing %q\nfull stderr:\n%s", want, got)
		}
	}
	if strings.Contains(got, "CI FAILED") {
		t.Errorf("require banner should not include generic CI FAILED header:\n%s", got)
	}
}

// TestIsCompileFailure checks the type-based gate in isolation.
func TestIsCompileFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"require", &RequireViolationError{Message: "x"}, true},
		{"registry", &MissingCredentialError{Hosts: []string{"ghcr.io"}}, true},
		{"buildkit", &BuildKitSecretEnvMissingError{Step: "s", SecretID: "i", EnvVar: "E"}, true},
		{"tag", &TagConfigError{Reason: "both"}, true},
		{"plain", errors.New("kaboom"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCompileFailure(tc.err); got != tc.want {
				t.Errorf("isCompileFailure(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
