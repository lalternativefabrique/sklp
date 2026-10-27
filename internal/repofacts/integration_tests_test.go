package repofacts

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// TestIsComposeFile pins the set of recognised compose filenames. The
// CI generator depends on compose.test.yml / compose.ci.yml being
// surfaced — Acme keeps its test stack there.
func TestIsComposeFile(t *testing.T) {
	yes := []string{
		"compose.yml", "compose.yaml",
		"docker-compose.yml", "docker-compose.yaml",
		"compose.test.yml", "compose.ci.yml", "compose.override.yaml",
		"docker-compose.test.yml", "docker-compose.override.yml",
	}
	for _, n := range yes {
		if !isComposeFile(n) {
			t.Errorf("expected %q to be a compose file", n)
		}
	}
	no := []string{
		"compose", "compose.txt", "stack.yml", "Dockerfile",
		"compose.test", "composeyml",
	}
	for _, n := range no {
		if isComposeFile(n) {
			t.Errorf("did not expect %q to be a compose file", n)
		}
	}
}

// TestDetectIntegrationTests_NameAndCompose: a *_integration_test.go
// file mentioning postgres + a compose.test.yml referencing nats must
// surface both as service hints, list the test file, and avoid setting
// the build tag flag (no `//go:build integration`).
func TestDetectIntegrationTests_NameAndCompose(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, root, "apps/core/pensee/integration_test.go",
		"package pensee\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) { _ = \"postgres://\" }\n")
	mustWrite(t, root, "compose.test.yml",
		"services:\n  nats-test:\n    image: nats:2.10\n")

	walked := []string{
		filepath.Join(root, "apps/core/pensee/integration_test.go"),
		filepath.Join(root, "compose.test.yml"),
	}
	got := detectIntegrationTests(walked, root, []string{"compose.test.yml"})

	if got.HasBuildTag {
		t.Errorf("no //go:build integration tag was present; HasBuildTag should be false")
	}
	if len(got.GoFiles) != 1 || got.GoFiles[0] != "apps/core/pensee/integration_test.go" {
		t.Errorf("expected integration test file to be surfaced, got %v", got.GoFiles)
	}
	wantHints := []string{"nats", "postgres"}
	sort.Strings(got.ServiceHints)
	if !reflect.DeepEqual(got.ServiceHints, wantHints) {
		t.Errorf("service hints: got %v, want %v", got.ServiceHints, wantHints)
	}
}

// TestDetectIntegrationTests_BuildTag covers the `//go:build integration`
// branch: the file doesn't carry the _integration_test.go suffix but
// must still be recognised as an integration test.
func TestDetectIntegrationTests_BuildTag(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, root, "pkg/db/db_test.go",
		"//go:build integration\n\npackage db\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) { _ = \"redis\" }\n")

	walked := []string{filepath.Join(root, "pkg/db/db_test.go")}
	got := detectIntegrationTests(walked, root, nil)

	if !got.HasBuildTag {
		t.Errorf("expected HasBuildTag=true for //go:build integration file")
	}
	if len(got.GoFiles) != 1 {
		t.Errorf("expected the build-tagged file to be listed, got %v", got.GoFiles)
	}
	if len(got.ServiceHints) != 1 || got.ServiceHints[0] != "redis" {
		t.Errorf("expected redis hint, got %v", got.ServiceHints)
	}
}

// TestDetectIntegrationTests_Empty: a repo with no tests/compose at
// all yields a zero-value struct (no false positives).
func TestDetectIntegrationTests_Empty(t *testing.T) {
	got := detectIntegrationTests(nil, t.TempDir(), nil)
	if got.HasBuildTag || len(got.GoFiles) > 0 || len(got.ServiceHints) > 0 {
		t.Errorf("expected zero-value IntegrationTestFacts, got %+v", got)
	}
}

func mustWrite(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}
