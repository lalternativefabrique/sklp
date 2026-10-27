package space

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeRuncForPs writes a fake runc that answers `list -q` from the state dir
// and `state <id>` with a minimal JSON carrying a "running" status — enough to
// drive ListDeployContainers without real containers. `kill <id> SIGTERM`
// removes the id's state entry, so killContainer's graceful-stop poll (which
// waits for "stopped" or the entry disappearing) sees an immediate exit
// instead of waiting out the full grace period — this fake models a
// container that always stops promptly, not one that hangs.
func fakeRuncForPs(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-runc")
	script := `#!/bin/sh
state=""
op=""
prev=""
for a in "$@"; do
  if [ "$prev" = "--root" ]; then state="$a"; fi
  if [ -z "$op" ] && [ "$prev" != "--root" ] && [ "${a#--}" = "$a" ]; then op="$a"; fi
  prev="$a"
done
id=""
for a in "$@"; do
  case "$a" in
    SIGTERM|SIGKILL) ;;
    *) id="$a" ;;
  esac
done
case "$op" in
  list) [ -d "$state" ] && ls -1 "$state" 2>/dev/null ;;
  state)
    if [ -n "$state" ] && [ ! -e "$state/$id" ]; then
      echo "{\"id\": \"$id\", \"status\": \"stopped\"}"
    else
      echo "{\"id\": \"$id\", \"status\": \"running\"}"
    fi
    ;;
  kill|delete) rm -f "$state/$id" ;;
esac
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake runc: %v", err)
	}
	return bin
}

// TestListDeployContainers_FiltersPrefix is the FIX-C guard: ListDeployContainers
// reports only the sklp-deploy-* containers in the deploy runc state, with their
// status, and never touches a foreign (e.g. CI) container sharing the binary's
// caches.
func TestListDeployContainers_FiltersPrefix(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("SKLP_RUNC_PATH", fakeRuncForPs(t))
	t.Setenv("SKLP_UNSAFE_RUNC_PATH", "1")

	// The state root ListDeployContainers reads.
	state := filepath.Join(cache, "sklp", "embedded-runtime", "deploy-runc-state")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"sklp-deploy-api-0", "sklp-deploy-db-0", "ci-leader-xyz"} {
		if err := os.WriteFile(filepath.Join(state, id), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got, err := ListDeployContainers("")
	if err != nil {
		t.Fatalf("ListDeployContainers: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("listed %d containers, want 2 (only sklp-deploy-*): %+v", len(got), got)
	}
	seen := map[string]string{}
	for _, c := range got {
		seen[c.ID] = c.Status
	}
	for _, want := range []string{"sklp-deploy-api-0", "sklp-deploy-db-0"} {
		if seen[want] != "running" {
			t.Fatalf("container %q status = %q, want running", want, seen[want])
		}
	}
	if _, leaked := seen["ci-leader-xyz"]; leaked {
		t.Fatalf("CI container leaked into deploy ps: %+v", got)
	}
}

// TestDeployInstanceID confirms the runc id mapping a space scope relies on:
// "<svc>-<i>" -> "sklp-deploy-<svc>-<i>".
func TestDeployInstanceID(t *testing.T) {
	if got, want := DeployInstanceID("", "api-0"), "sklp-deploy-api-0"; got != want {
		t.Fatalf("DeployInstanceID = %q, want %q", got, want)
	}
}
