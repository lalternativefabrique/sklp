package space

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, dir, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRepoFromBundle(t *testing.T) {
	dir := t.TempDir()

	withWS := writeConfig(t, filepath.Join(dir, "svc-runner"), `{
	  "mounts": [
	    {"destination": "/proc", "type": "proc", "source": "proc"},
	    {"destination": "/workspace", "type": "bind", "source": "/home/u/code/projects/acme"}
	  ]
	}`)
	if got := repoFromBundle(withWS); got != "/home/u/code/projects/acme" {
		t.Errorf("repoFromBundle (with /workspace) = %q", got)
	}

	noWS := writeConfig(t, filepath.Join(dir, "svc-nows"), `{"mounts":[{"destination":"/tmp","source":"tmpfs"}]}`)
	if got := repoFromBundle(noWS); got != "" {
		t.Errorf("repoFromBundle (no /workspace) = %q, want empty", got)
	}

	if got := repoFromBundle(filepath.Join(dir, "absent", "config.json")); got != "" {
		t.Errorf("repoFromBundle (missing file) = %q, want empty", got)
	}

	bad := writeConfig(t, filepath.Join(dir, "svc-bad"), `not json`)
	if got := repoFromBundle(bad); got != "" {
		t.Errorf("repoFromBundle (bad json) = %q, want empty", got)
	}
}
