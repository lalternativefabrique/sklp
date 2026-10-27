package space

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// writeProcessFile must keep secret values off runc's argv by routing them
// through a 0600 OCI Process JSON (`runc exec -p`). This guards the leak fix: env
// passed to `runc exec` used to be per-var --env flags, visible in
// /proc/<pid>/cmdline to any local `ps`. (runc exec has no --env-file flag.)
func TestWriteProcessFile(t *testing.T) {
	env := []string{"SKLP_RUNNER_MODE=embedded-oci", "SCW_SECRET_KEY=ad8cfd59-secret"}
	argv := []string{"sklp", "run", "ci"}
	// A non-existent runc binary makes bundlePath() return "" so the Process is
	// built from scratch (the production base-from-config.json path is exercised
	// in the integration build); the JSON shape and perms are what matter here.
	path, cleanup, err := writeProcessFile("/nonexistent/runc", t.TempDir(), "svc-x", env, argv)
	if err != nil {
		t.Fatalf("writeProcessFile: %v", err)
	}
	defer cleanup()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("perm = %o, want 600", perm)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var proc struct {
		Args     []string `json:"args"`
		Env      []string `json:"env"`
		Terminal bool     `json:"terminal"`
	}
	if err := json.Unmarshal(b, &proc); err != nil {
		t.Fatalf("process json invalid: %v", err)
	}
	if len(proc.Args) != 3 || proc.Args[0] != "sklp" {
		t.Errorf("args = %v, want the supplied argv", proc.Args)
	}
	if len(proc.Env) != 2 || proc.Env[1] != "SCW_SECRET_KEY=ad8cfd59-secret" {
		t.Errorf("env = %v, want the merged secret env", proc.Env)
	}
	if proc.Terminal {
		t.Error("terminal = true, want false on the non-interactive exec path")
	}

	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("cleanup did not remove file: %v", err)
	}
}

// mergeProcEnv overlays caller env onto the base config.json env, the caller
// winning on a key collision (a baked default must be replaceable by a secret).
func TestMergeProcEnv(t *testing.T) {
	base := []any{"PATH=/usr/bin", "SKLP_RUNNER_MODE=baked"}
	got := mergeProcEnv(base, []string{"SKLP_RUNNER_MODE=embedded-oci", "TOKEN=s3cr3t"})

	want := map[string]string{
		"PATH":             "/usr/bin",
		"SKLP_RUNNER_MODE": "embedded-oci", // overridden, not duplicated
		"TOKEN":            "s3cr3t",
	}
	if len(got) != len(want) {
		t.Fatalf("env = %v, want %d entries (no duplicate keys)", got, len(want))
	}
	m := map[string]string{}
	for _, kv := range got {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %q, want %q (full: %v)", k, m[k], v, got)
		}
	}
}
