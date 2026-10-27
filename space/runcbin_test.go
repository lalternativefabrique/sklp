package space

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// fakeRunc writes a shell script that records its argv to a file, so we can
// assert the runtime invokes the path we pass rather than a PATH lookup.
func fakeRunc(t *testing.T) (binPath, logPath string) {
	t.Helper()
	dir := t.TempDir()
	binPath = filepath.Join(dir, "fake-runc")
	logPath = filepath.Join(dir, "argv.log")
	script := "#!/bin/sh\necho \"$0 $@\" >> " + logPath + "\nexit 0\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake runc: %v", err)
	}
	return binPath, logPath
}

// TestDeleteContainerUsesProvidedBin proves the runc invocations are routed to
// the passed binary path (the extracted embedded runc), not the literal "runc"
// on PATH. If any call site regressed to "runc", the fake would never be hit.
func TestDeleteContainerUsesProvidedBin(t *testing.T) {
	bin, log := fakeRunc(t)

	if err := deleteContainer(bin, "/tmp/state", "some-id"); err != nil {
		t.Fatalf("deleteContainer: %v", err)
	}
	killContainer(bin, "/tmp/state", "some-id")
	if err := runcExec(context.Background(), bin, "/tmp/state", "some-id", "true"); err != nil {
		t.Fatalf("runcExec: %v", err)
	}

	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("fake runc was never invoked (read log: %v)", err)
	}
	if len(data) == 0 {
		t.Fatal("fake runc produced no argv records")
	}
}

// TestContainerLogIsPrivate proves runForeground writes its stdio capture
// 0600, never world-readable. The log can echo secret env (svc.Env, e.g. a
// connection string with POSTGRES_PASSWORD), so a regression to os.Create
// (0644) would leak secrets on disk — the same class as the bundle
// config.json finding fixed in spec.go.
func TestContainerLogIsPrivate(t *testing.T) {
	bin, _ := fakeRunc(t)
	dir := t.TempDir()
	logPath := filepath.Join(dir, "service.log")

	if _, err := runForeground(context.Background(), bin, "/tmp/state", dir, "id", logPath); err != nil {
		t.Fatalf("runForeground: %v", err)
	}
	fi, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat log: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("service log mode = %o, want 0600 (world-readable log leaks secret env)", perm)
	}
}

// TestResolveRuncOverride checks SKLP_RUNC_PATH wins over extraction.
func TestResolveRuncOverride(t *testing.T) {
	bin, _ := fakeRunc(t)
	t.Setenv("SKLP_RUNC_PATH", bin)
	t.Setenv("SKLP_UNSAFE_RUNC_PATH", "1")

	got, err := resolveRunc(t.TempDir())
	if err != nil {
		t.Fatalf("resolveRunc: %v", err)
	}
	if got != bin {
		t.Fatalf("resolveRunc = %q, want override %q", got, bin)
	}
}

func TestResolveRuncOverrideMissing(t *testing.T) {
	t.Setenv("SKLP_RUNC_PATH", "/nonexistent/runc")
	t.Setenv("SKLP_UNSAFE_RUNC_PATH", "1")
	if _, err := resolveRunc(t.TempDir()); err == nil {
		t.Fatal("expected error for missing SKLP_RUNC_PATH binary")
	}
}

func TestResolveRuncOverrideRequiresUnsafeOptIn(t *testing.T) {
	bin, _ := fakeRunc(t)
	t.Setenv("SKLP_RUNC_PATH", bin)
	if _, err := resolveRunc(t.TempDir()); err == nil {
		t.Fatal("expected SKLP_RUNC_PATH to require SKLP_UNSAFE_RUNC_PATH=1")
	}
}

func TestResolveRuncOverrideRejectsRelativePath(t *testing.T) {
	t.Setenv("SKLP_RUNC_PATH", "runc")
	t.Setenv("SKLP_UNSAFE_RUNC_PATH", "1")
	if _, err := resolveRunc(t.TempDir()); err == nil {
		t.Fatal("expected relative SKLP_RUNC_PATH to be rejected")
	}
}

func TestResolveRuncOverrideRejectsNonExecutable(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "runc")
	if err := os.WriteFile(bin, []byte("x"), 0o600); err != nil {
		t.Fatalf("write non-exec runc: %v", err)
	}
	t.Setenv("SKLP_RUNC_PATH", bin)
	t.Setenv("SKLP_UNSAFE_RUNC_PATH", "1")
	if _, err := resolveRunc(t.TempDir()); err == nil {
		t.Fatal("expected non-executable SKLP_RUNC_PATH to be rejected")
	}
}

// fakeRuncWithState writes a fake runc that emulates the subset reapAllContainers
// uses: `list -q` prints the basename of each file in <state> (one per
// "container"), and `delete`/`kill` remove that file. It records argv to a log
// so a test asserts which ids were reaped. The state dir IS the container set.
func fakeRuncWithState(t *testing.T, state string) (binPath, logPath string) {
	t.Helper()
	dir := t.TempDir()
	binPath = filepath.Join(dir, "fake-runc")
	logPath = filepath.Join(dir, "argv.log")
	// Args layout: --root <state> <op> [flags...] <id>. The id is the last arg.
	script := `#!/bin/sh
echo "$@" >> ` + logPath + `
state=""
op=""
prev=""
for a in "$@"; do
  if [ "$prev" = "--root" ]; then state="$a"; fi
  if [ -z "$op" ] && [ "$prev" != "--root" ] && [ "${a#--}" = "$a" ]; then op="$a"; fi
  prev="$a"
done
id=""
for a in "$@"; do id="$a"; done
case "$op" in
  list)
    [ -d "$state" ] && ls -1 "$state" 2>/dev/null
    ;;
  delete|kill)
    rm -f "$state/$id"
    ;;
esac
exit 0
`
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake runc: %v", err)
	}
	return binPath, logPath
}

// TestReapAllContainers_RemovesEveryContainer is the FIX-3 guard: every
// container in the runc state — including a leaked one no current process
// tracks — is force-deleted, leaving no residue. The state dir models the runc
// state; reaping must empty it.
func TestReapAllContainers_RemovesEveryContainer(t *testing.T) {
	state := t.TempDir()
	// Two leaked deploy containers + one unrelated container.
	for _, id := range []string{"sklp-deploy-db-0", "sklp-deploy-hello-0", "ci-leader-xyz"} {
		if err := os.WriteFile(filepath.Join(state, id), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bin, _ := fakeRuncWithState(t, state)

	// Prefix-scoped reap: only the deploy containers go, the CI one stays.
	reapAllContainers(bin, state, "sklp-deploy-")

	left := listContainers(bin, state)
	if len(left) != 1 || left[0] != "ci-leader-xyz" {
		t.Fatalf("after prefix reap, remaining = %v, want only [ci-leader-xyz]", left)
	}

	// Unscoped reap removes the rest.
	reapAllContainers(bin, state, "")
	if left := listContainers(bin, state); len(left) != 0 {
		t.Fatalf("after full reap, remaining = %v, want none (no leak)", left)
	}
}

// TestListContainers_EmptyState confirms an empty/missing state lists nothing
// (no error, no spurious id) — a first deploy must not trip on it.
func TestListContainers_EmptyState(t *testing.T) {
	state := t.TempDir()
	bin, _ := fakeRuncWithState(t, state)
	if got := listContainers(bin, state); len(got) != 0 {
		t.Fatalf("empty state listed %v, want none", got)
	}
}
