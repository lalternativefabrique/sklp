package space

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFirstRunningContainerID is the exec-fallback guard: when a space has no
// `space up` init, exec targets the first running container of the space. This
// covers spaces launched by dev/ci/deploy whose containers carry service names,
// not the `shell` init — the case where exec used to wrongly report "not
// running" on a space `ls` shows as running.
func TestFirstRunningContainerID(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("SKLP_RUNC_PATH", fakeRuncForPs(t))
	t.Setenv("SKLP_UNSAFE_RUNC_PATH", "1")

	// A named space's scoped state root (spaces/<name>/runc-state), with two
	// service-named containers — no `shell` init among them.
	name := "demo"
	state := filepath.Join(cache, "sklp", "embedded-runtime", "spaces", name, "runc-state")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"sklp-deploy-demo-nats-0", "sklp-deploy-demo-web-0"} {
		if err := os.WriteFile(filepath.Join(state, id), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got := firstRunningContainerID(name)
	if got == "" {
		t.Fatal("firstRunningContainerID returned empty; want a running container id")
	}
	// The fake reports every listed container as running; the result must be one
	// of the space's containers (raw id, not the absent `shell` init).
	if got != "sklp-deploy-demo-nats-0" && got != "sklp-deploy-demo-web-0" {
		t.Fatalf("got %q, want one of the demo containers", got)
	}

	// An empty space yields no fallback target.
	if id := firstRunningContainerID("nonexistent"); id != "" {
		t.Fatalf("empty space returned %q, want \"\"", id)
	}
}
