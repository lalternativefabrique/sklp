//go:build linux

package spacenet

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// fixtureCgroups points the reapers at a temp tree shaped like the systemd user
// slice and returns its base. Each entry is a cgroup basename mapped to the pids
// its cgroup.procs holds (empty slice → an empty cgroup).
func fixtureCgroups(t *testing.T, cgroups map[string][]string) string {
	t.Helper()
	base := filepath.Join(t.TempDir(), "user@1000.service")
	if err := os.MkdirAll(filepath.Join(base, "app.slice"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, pids := range cgroups {
		dir := filepath.Join(base, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := ""
		for _, p := range pids {
			body += p + "\n"
		}
		if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	prev := cgroupUserRoot
	cgroupUserRoot = func() string { return base }
	t.Cleanup(func() { cgroupUserRoot = prev })
	return base
}

// recorder collects the kill/rmdir calls a reap pass makes.
type recorder struct {
	killed []int
	rmdirs []string
}

func (r *recorder) kill(pid int) error   { r.killed = append(r.killed, pid); return nil }
func (r *recorder) rmdir(d string) error { r.rmdirs = append(r.rmdirs, filepath.Base(d)); return nil }

// fixtureProc makes procCmdline serve the given NUL-separated argv for one pid.
func fixtureProc(t *testing.T, cmdline string) int {
	t.Helper()
	const pid = 4242
	prev := procCmdline
	procCmdline = func(p int) ([]byte, error) {
		if p != pid {
			return nil, os.ErrNotExist
		}
		return []byte(cmdline), nil
	}
	t.Cleanup(func() { procCmdline = prev })
	return pid
}

// The runner's pause is bind-mounted at /.sklp/pause, so its arg0 basename is
// "pause", not "sklp". Matching only "sklp" left every bind-mounted runner's
// cgroup unreclaimable.
func TestIsResidualPauseAcceptsBindMountedPause(t *testing.T) {
	for _, cmdline := range []string{
		"/.sklp/pause\x00pause\x00",
		"/usr/local/bin/sklp\x00pause\x00",
	} {
		pid := fixtureProc(t, cmdline)
		if !isResidualPause(pid) {
			t.Fatalf("cmdline %q not recognised as a residual pause", cmdline)
		}
	}
}

func TestIsResidualPauseRejectsRealWorkload(t *testing.T) {
	pid := fixtureProc(t, "/usr/local/bin/nats-server\x00-js\x00")
	if isResidualPause(pid) {
		t.Fatal("nats-server recognised as a pause; a real workload must never be reaped")
	}
}

// A leaked service holds its cgroup with a real process (not a pause), so the
// conservative reaper must refuse it — that refusal is what made the CI leak
// unfixable without a manual kill.
func TestReapOrphanCgroupsKeepsRealWorkload(t *testing.T) {
	fixtureCgroups(t, map[string][]string{
		"sklp-deploy-ci-abc12345-nats-test": {"999999"},
	})
	var rec recorder
	res := ReapOrphanCgroups(nil, false, rec.kill, rec.rmdir)

	if len(rec.killed) != 0 {
		t.Fatalf("killed %v, want none: a non-pause proc must never be reaped here", rec.killed)
	}
	if !slices.Contains(res.Protected, "sklp-deploy-ci-abc12345-nats-test") {
		t.Fatalf("Protected = %v, want the workload cgroup", res.Protected)
	}
}

// ReapCgroupsByPrefix is the recovery path: the caller has established the space
// is being torn down, so whatever holds the cgroup dies.
func TestReapCgroupsByPrefixKillsRealWorkload(t *testing.T) {
	fixtureCgroups(t, map[string][]string{
		"sklp-deploy-ci-abc12345-nats-test": {"4101633"},
		"sklp-deploy-other-abc-postgres":    {"4113260"},
	})
	var rec recorder
	res := ReapCgroupsByPrefix("sklp-deploy-ci-abc12345-", nil, false, rec.kill, rec.rmdir)

	if !slices.Equal(rec.killed, []int{4101633}) {
		t.Fatalf("killed %v, want only the prefixed space's pid", rec.killed)
	}
	if !slices.Contains(res.EmptyRemoved, "sklp-deploy-ci-abc12345-nats-test") {
		t.Fatalf("EmptyRemoved = %v, want the drained cgroup", res.EmptyRemoved)
	}
	if slices.Contains(rec.rmdirs, "sklp-deploy-other-abc-postgres") {
		t.Fatal("another space's cgroup was removed; the prefix must scope the reap")
	}
}

func TestReapCgroupsByPrefixIgnoresEmptyPrefix(t *testing.T) {
	fixtureCgroups(t, map[string][]string{"sklp-deploy-ci-abc12345-nats-test": {"4101633"}})
	var rec recorder
	ReapCgroupsByPrefix("", nil, false, rec.kill, rec.rmdir)

	if len(rec.killed) != 0 {
		t.Fatalf("killed %v, want none: an empty prefix must not match every space", rec.killed)
	}
}

// A cgroup whose space still exists is reachable by the normal teardown, so
// prune must leave it alone; one whose space is gone has no other remedy.
func TestReapUnownedCgroupsSkipsKnownSpaces(t *testing.T) {
	fixtureCgroups(t, map[string][]string{
		"sklp-deploy-live-11112222-postgres": {"111"},
		"sklp-deploy-ci-abc12345-nats-test":  {"222"},
	})
	var rec recorder
	res := ReapUnownedCgroups([]string{"sklp-deploy-live-11112222-"}, nil, false, rec.kill, rec.rmdir)

	if !slices.Equal(rec.killed, []int{222}) {
		t.Fatalf("killed %v, want only the orphaned space's pid", rec.killed)
	}
	if !slices.Contains(res.EmptyRemoved, "sklp-deploy-ci-abc12345-nats-test") {
		t.Fatalf("EmptyRemoved = %v, want the orphaned cgroup", res.EmptyRemoved)
	}
}

func TestReapUnownedCgroupsHonoursProtected(t *testing.T) {
	fixtureCgroups(t, map[string][]string{"sklp-deploy-ci-abc12345-nats-test": {"222"}})
	var rec recorder
	protected := map[string]bool{"sklp-deploy-ci-abc12345-nats-test": true}
	ReapUnownedCgroups(nil, protected, false, rec.kill, rec.rmdir)

	if len(rec.killed) != 0 {
		t.Fatalf("killed %v, want none: a live container must stay protected", rec.killed)
	}
}

func TestReapUnownedCgroupsDryRunMutatesNothing(t *testing.T) {
	base := fixtureCgroups(t, map[string][]string{"sklp-deploy-ci-abc12345-nats-test": {"222"}})
	var rec recorder
	res := ReapUnownedCgroups(nil, nil, true, rec.kill, rec.rmdir)

	if len(rec.killed) != 0 || len(rec.rmdirs) != 0 {
		t.Fatalf("dry run mutated: killed=%v rmdirs=%v", rec.killed, rec.rmdirs)
	}
	if !slices.Contains(res.Killed, 222) {
		t.Fatalf("Killed = %v, want the pid it would have killed", res.Killed)
	}
	if _, err := os.Stat(filepath.Join(base, "sklp-deploy-ci-abc12345-nats-test")); err != nil {
		t.Fatalf("dry run removed the cgroup dir: %v", err)
	}
}

func TestCgroupProcsForNamesTheOffendingPids(t *testing.T) {
	fixtureCgroups(t, map[string][]string{"sklp-deploy-ci-abc12345-nats-test": {"4101633", "4113260"}})

	got := CgroupProcsFor("sklp-deploy-ci-abc12345-nats-test")
	if !slices.Equal(got, []int{4101633, 4113260}) {
		t.Fatalf("CgroupProcsFor = %v, want both pids", got)
	}
	if got := CgroupProcsFor("sklp-deploy-absent"); got != nil {
		t.Fatalf("CgroupProcsFor(absent) = %v, want nil", got)
	}
}
