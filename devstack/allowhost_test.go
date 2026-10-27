package devstack

import (
	"os"
	"path/filepath"
	"testing"
)

func writeDevYAML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dev.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestAllowHost_DefaultsClosed is the security default: a stack that says
// nothing gets no host access.
func TestAllowHost_DefaultsClosed(t *testing.T) {
	path := writeDevYAML(t, `version: 1
kind: devstack
services:
  api: { run: "echo a" }
`)
	stack, err := LoadOpts(path, false)
	if err != nil {
		t.Fatalf("LoadOpts: %v", err)
	}
	if stack.AllowHost {
		t.Fatal("allowHost must default to false")
	}
}

func TestAllowHost_ParsedWhenDeclared(t *testing.T) {
	path := writeDevYAML(t, `version: 1
kind: devstack
allowHost: true
services:
  api: { run: "echo a" }
`)
	stack, err := LoadOpts(path, false)
	if err != nil {
		t.Fatalf("LoadOpts: %v", err)
	}
	if !stack.AllowHost {
		t.Fatal("allowHost: true not carried into the Stack")
	}
}

// TestAllowHost_IgnoredOutsideDevstack pins the dev-only scope. The pipeline
// loader is permissive, so a stray allowHost in a CI/publish YAML parses — but
// nothing on those paths reads it and Space.allowHost stays false, so the
// egress filter there is unchanged. Only a devstack document grants anything.
func TestAllowHost_IgnoredOutsideDevstack(t *testing.T) {
	path := writeDevYAML(t, `version: 1
kind: pipeline
allowHost: true
steps:
  - name: a
    run: "echo a"
`)
	if _, err := LoadOpts(path, false); err == nil {
		t.Fatal("a non-devstack kind must not load as a dev stack")
	}
}

// TestAllowHost_RejectedPerService pins the field to the document root. The
// blackhole lives in the space netns shared by every service, so a per-service
// grant cannot be honoured — accepting the key there would be a lie.
func TestAllowHost_RejectedPerService(t *testing.T) {
	path := writeDevYAML(t, `version: 1
kind: devstack
services:
  api:
    run: "echo a"
    allowHost: true
`)
	if _, err := strictParse(path, false); err == nil {
		t.Fatal("per-service allowHost must be rejected: the exemption is space-wide")
	}
}
