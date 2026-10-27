package devstack

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Typo `dependsOn` → expect autoFix to rewrite it to `depends_on`
// and return without error.
func TestStrictParse_AutoFixesCamelCaseTypo(t *testing.T) {
	dir := t.TempDir()
	src := []byte(`version: 1
kind: devstack
services:
  api: { run: "echo a" }
  web:
    run: "echo w"
    dependsOn: [api]
`)
	path := filepath.Join(dir, "dev.yaml")
	if err := os.WriteFile(path, src, 0o644); err != nil {
		t.Fatal(err)
	}
	applied, err := strictParse(path, true)
	if err != nil {
		t.Fatalf("expected auto-fix to succeed, got error: %v", err)
	}
	if !applied {
		t.Fatal("expected applied=true")
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "depends_on:") {
		t.Fatalf("expected file to contain depends_on after fix, got:\n%s", got)
	}
	if strings.Contains(string(got), "dependsOn:") {
		t.Fatalf("expected dependsOn to be removed, got:\n%s", got)
	}
}

// With autoFix=false, the same file should produce a friendly error
// that names the YAML path + the suggestion (and never mentions Go
// type names like devstackSvcDoc).
func TestStrictParse_NoFixReportsFriendlyError(t *testing.T) {
	dir := t.TempDir()
	src := []byte(`version: 1
kind: devstack
services:
  web:
    run: "echo w"
    dependsOn: [api]
`)
	path := filepath.Join(dir, "dev.yaml")
	_ = os.WriteFile(path, src, 0o644)
	_, err := strictParse(path, false)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "unknown field: dependsOn") {
		t.Errorf("missing field name in error: %s", msg)
	}
	if !strings.Contains(msg, "depends_on") {
		t.Errorf("missing suggestion in error: %s", msg)
	}
	if strings.Contains(msg, "devstackSvcDoc") || strings.Contains(msg, "runner.") {
		t.Errorf("error leaked Go type name: %s", msg)
	}
}

// Garbage field with no close match should NOT be auto-fixed (avoid
// wild guesses).
func TestStrictParse_NoFixForLowConfidence(t *testing.T) {
	dir := t.TempDir()
	src := []byte(`version: 1
kind: devstack
services:
  web:
    run: "echo w"
    xyzzy: true
`)
	path := filepath.Join(dir, "dev.yaml")
	_ = os.WriteFile(path, src, 0o644)
	applied, err := strictParse(path, true)
	if err == nil {
		t.Fatal("expected error for unknown field with no good match")
	}
	if applied {
		t.Error("should not have applied a fix")
	}
}

// A file with no errors must be a no-op (no rewrite, no error).
func TestStrictParse_HappyPath(t *testing.T) {
	dir := t.TempDir()
	src := []byte(`version: 1
kind: devstack
services:
  api: { run: "echo a" }
  web:
    run: "echo w"
    depends_on: [api]
`)
	path := filepath.Join(dir, "dev.yaml")
	_ = os.WriteFile(path, src, 0o644)
	applied, err := strictParse(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Error("should not have rewritten a clean file")
	}
}
