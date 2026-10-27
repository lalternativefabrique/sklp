package spec

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePipeline(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestUnknownStepFieldIsRejected(t *testing.T) {
	path := writePipeline(t, "ci.yaml", `version: 1
kind: pipeline
steps:
  - name: facade-test
    workdir: apps/core
    run: go build ./...
`)
	_, err := LoadPipeline(path)
	if err == nil {
		t.Fatal("expected unknown field workdir to be rejected")
	}
	var ue *UnknownKeyError
	if !errors.As(err, &ue) {
		t.Fatalf("want *UnknownKeyError, got %T", err)
	}
	if ue.Key != "workdir" {
		t.Errorf("Key = %q, want workdir", ue.Key)
	}
	if ue.Suggestion != "cwd" {
		t.Errorf("Suggestion = %q, want cwd", ue.Suggestion)
	}
	if ue.Line != 5 {
		t.Errorf("Line = %d, want 5", ue.Line)
	}
	msg := err.Error()
	for _, want := range []string{"facade-test", "did you mean `cwd:`", "ci.yaml:5"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
}

func TestUnknownPipelineFieldIsRejected(t *testing.T) {
	path := writePipeline(t, "ci.yaml", `version: 1
kind: pipeline
toolz:
  go: "1.25.0"
steps:
  - name: t
    run: go test ./...
`)
	_, err := LoadPipeline(path)
	if err == nil {
		t.Fatal("expected unknown root field to be rejected")
	}
	var ue *UnknownKeyError
	if !errors.As(err, &ue) {
		t.Fatalf("want *UnknownKeyError, got %T", err)
	}
	if ue.Suggestion != "tools" {
		t.Errorf("Suggestion = %q, want tools", ue.Suggestion)
	}
}

func TestUnknownFieldInExtendedTemplateNamesThatFile(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "_base.yaml")
	if err := os.WriteFile(base, []byte("version: 1\nkind: pipeline\nrunnr: base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(dir, "ci.yaml")
	if err := os.WriteFile(child, []byte("version: 1\nextends: base\nsteps:\n  - name: t\n    run: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadPipeline(child)
	if err == nil {
		t.Fatal("expected the template's unknown field to be rejected")
	}
	if !strings.Contains(err.Error(), "_base.yaml") {
		t.Errorf("error should name the template that holds the typo:\n%s", err)
	}
}

func TestValidPipelineStillLoads(t *testing.T) {
	path := writePipeline(t, "ci.yaml", `version: 1
kind: pipeline
scopes:
  facade: ["apps/core/**"]
tools:
  go: "1.25.0"
steps:
  - name: facade-test
    scope: [facade]
    tools: [go]
    cwd: apps/core
    run: go build ./...
`)
	p, err := LoadPipeline(path)
	if err != nil {
		t.Fatalf("valid pipeline rejected: %v", err)
	}
	if p.Steps[0].Cwd != "apps/core" {
		t.Errorf("Cwd = %q, want apps/core", p.Steps[0].Cwd)
	}
}

func TestFreeFormFieldsKeepArbitraryKeys(t *testing.T) {
	path := writePipeline(t, "publish.yaml", `version: 1
kind: pipeline
targets:
  anything: {a: 1}
service_map:
  core: apps/core
steps:
  - name: t
    run: true
`)
	if _, err := LoadPipeline(path); err != nil {
		t.Fatalf("free-form maps must accept any key: %v", err)
	}
}
