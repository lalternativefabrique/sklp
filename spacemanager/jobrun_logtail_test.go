package spacemanager

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// capturingExecLauncher is execLauncher plus the one capability the real
// runtime has and the plain exec launcher lacks: it redirects the child's
// stdout/stderr into a per-instance file opened O_TRUNC on every launch, and
// exposes it through LogPath. That makes the agent's log streamer live in
// tests, which is the only way to exercise what a job run actually records.
type capturingExecLauncher struct {
	dir    string
	mu     sync.Mutex
	pids   map[string]int
	starts map[string]int
}

func newCapturing(dir string) *capturingExecLauncher {
	return &capturingExecLauncher{
		dir:    dir,
		pids:   make(map[string]int),
		starts: make(map[string]int),
	}
}

func (c *capturingExecLauncher) LogPath(id string) string {
	return filepath.Join(c.dir, id+".log")
}

func (c *capturingExecLauncher) Start(_ context.Context, svc Service) (int, error) {
	if len(svc.Command) == 0 {
		return 0, fmt.Errorf("capturing launcher: service %q has no command", svc.Name)
	}
	f, err := os.OpenFile(c.LogPath(svc.Name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	cmd := exec.Command(svc.Command[0], svc.Command[1:]...)
	cmd.Env = svc.Env
	cmd.Stdout = f
	cmd.Stderr = f
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("capturing launcher: start %q: %w", svc.Name, err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	c.mu.Lock()
	c.pids[svc.Name] = pid
	c.starts[svc.Name]++
	c.mu.Unlock()
	return pid, nil
}

func (c *capturingExecLauncher) Stop(id string) {
	c.mu.Lock()
	pid := c.pids[id]
	delete(c.pids, id)
	c.mu.Unlock()
	if pid > 0 {
		_ = unix.Kill(pid, unix.SIGKILL)
	}
}

func (c *capturingExecLauncher) Addr(id string) string { return "exec://" + id }

func (c *capturingExecLauncher) startCount(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.starts[name]
}

// logSinkOpts is testOpts plus a live LogSink, which is what makes the agent
// build an enabled logStreamer.
func logSinkOpts() Options {
	o := testOpts()
	o.LogSink = func(service, line string) {}
	return o
}

// TestJobRunLogTail_ShortJobStillRecordsItsOutput is the missing-logs guard: a
// migration that prints and exits well inside the tailer's poll interval used
// to be recorded with an empty tail (nothing buffered yet) or, worse, the
// previous run's tail. The file already holds the whole run, so the record must
// carry it.
func TestJobRunLogTail_ShortJobStillRecordsItsOutput(t *testing.T) {
	l := newCapturing(t.TempDir())
	desired := []Service{{
		Name:    "migrate",
		Oneshot: true,
		Command: []string{"sh", "-c", "echo '27/u jwks'; exit 0"},
	}}
	a, cancel, wg := startNewAgent(t, l, nil, desired, logSinkOpts())
	defer wg.Wait()
	defer cancel()

	waitFor(t, 5*time.Second, "run recorded", func() bool { return len(a.JobRuns("migrate")) == 1 })

	r := a.JobRuns("migrate")[0]
	if len(r.LogTail) == 0 {
		t.Fatal("a job that printed a line was recorded with an EMPTY tail: the missing-logs bug")
	}
	if !strings.Contains(strings.Join(r.LogTail, "\n"), "27/u jwks") {
		t.Fatalf("LogTail = %v, want it to contain the line the job printed", r.LogTail)
	}
}

// TestJobRunLogTail_RerunDoesNotInheritThePreviousTail is the regression guard
// for the reported symptom: two runs of the same oneshot printing DIFFERENT
// lines, where the second was reported with the first's output — a migration
// log showing the previous version's steps, which reads as if the new image
// never ran.
func TestJobRunLogTail_RerunDoesNotInheritThePreviousTail(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran-once")
	l := newCapturing(dir)
	// First run prints the old migration, every later run prints the new one.
	desired := []Service{{
		Name:    "migrate",
		Oneshot: true,
		Command: []string{"sh", "-c",
			"if [ -f " + marker + " ]; then echo '27/u jwks'; else touch " + marker + "; echo '26/u user_identity_id'; fi; exit 0"},
	}}
	a, cancel, wg := startNewAgent(t, l, nil, desired, logSinkOpts())
	defer wg.Wait()
	defer cancel()

	waitFor(t, 5*time.Second, "first run recorded", func() bool { return len(a.JobRuns("migrate")) == 1 })
	if got := strings.Join(a.JobRuns("migrate")[0].LogTail, "\n"); !strings.Contains(got, "26/u") {
		t.Fatalf("first run tail = %q, want the first migration", got)
	}

	if !a.TriggerJob("migrate") {
		t.Fatal("TriggerJob returned false for a Done job")
	}
	waitFor(t, 5*time.Second, "second run recorded", func() bool { return len(a.JobRuns("migrate")) == 2 })

	second := strings.Join(a.JobRuns("migrate")[1].LogTail, "\n")
	if strings.Contains(second, "26/u user_identity_id") {
		t.Fatalf("second run inherited the PREVIOUS run's tail (%q): the reported bug", second)
	}
	if !strings.Contains(second, "27/u jwks") {
		t.Fatalf("second run tail = %q, want its own output", second)
	}
	// The first record must still hold its own output, unchanged by the re-run.
	if got := strings.Join(a.JobRuns("migrate")[0].LogTail, "\n"); !strings.Contains(got, "26/u") {
		t.Fatalf("first run tail was overwritten by the re-run: %q", got)
	}
}

// TestJobRunLogTail_SilentJobReportsNoOutput: a job that prints nothing must be
// recorded with an empty tail rather than borrowing another run's lines. This is
// what makes the UI's "no output captured for this run" honest.
func TestJobRunLogTail_SilentJobReportsNoOutput(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran-once")
	l := newCapturing(dir)
	// First run is chatty, the re-run is silent.
	desired := []Service{{
		Name:    "migrate",
		Oneshot: true,
		Command: []string{"sh", "-c",
			"if [ -f " + marker + " ]; then :; else touch " + marker + "; echo 'no change'; fi; exit 0"},
	}}
	a, cancel, wg := startNewAgent(t, l, nil, desired, logSinkOpts())
	defer wg.Wait()
	defer cancel()

	waitFor(t, 5*time.Second, "first run recorded", func() bool { return len(a.JobRuns("migrate")) == 1 })
	if !a.TriggerJob("migrate") {
		t.Fatal("TriggerJob returned false for a Done job")
	}
	waitFor(t, 5*time.Second, "second run recorded", func() bool { return len(a.JobRuns("migrate")) == 2 })

	if got := a.JobRuns("migrate")[1].LogTail; len(got) != 0 {
		t.Fatalf("silent re-run tail = %v, want empty rather than the previous run's lines", got)
	}
}

// TestJobRunLogTail_FailedRunKeepsItsOwnOutput: the tail matters most on a
// failure, where it is the only explanation of the exit code.
func TestJobRunLogTail_FailedRunKeepsItsOwnOutput(t *testing.T) {
	l := newCapturing(t.TempDir())
	desired := []Service{{
		Name:    "migrate",
		Oneshot: true,
		Command: []string{"sh", "-c", "echo 'migration 27 failed: duplicate version'; exit 3"},
	}}
	a, cancel, wg := startNewAgent(t, l, nil, desired, logSinkOpts())
	defer wg.Wait()
	defer cancel()

	waitFor(t, 8*time.Second, "failed run recorded", func() bool { return len(a.JobRuns("migrate")) >= 1 })

	r := a.JobRuns("migrate")[0]
	if !r.Failed || r.ExitCode != 3 {
		t.Fatalf("run reports exit %d failed=%v, want exit 3 failed", r.ExitCode, r.Failed)
	}
	if !strings.Contains(strings.Join(r.LogTail, "\n"), "duplicate version") {
		t.Fatalf("failed run tail = %v, want the reason it failed", r.LogTail)
	}
}

// A chatty migration prints far more than maxRunLines. The tail must keep
// the END of the run (where the failure and the last applied version are), not
// the beginning.
func TestEdge_ChattyJobKeepsTheEndOfTheRun(t *testing.T) {
	l := newCapturing(t.TempDir())
	desired := []Service{{
		Name:    "migrate",
		Oneshot: true,
		Command: []string{"sh", "-c", "i=1; while [ $i -le 250 ]; do echo \"no change $i\"; i=$((i+1)); done; echo 'FINAL 27/u jwks'; exit 0"},
	}}
	a, cancel, wg := startNewAgent(t, l, nil, desired, logSinkOpts())
	defer wg.Wait()
	defer cancel()

	waitFor(t, 5*time.Second, "run recorded", func() bool { return len(a.JobRuns("migrate")) == 1 })
	tail := a.JobRuns("migrate")[0].LogTail
	joined := strings.Join(tail, "\n")
	t.Logf("tail len=%d: %v", len(tail), tail)
	if len(tail) > maxRunLines {
		t.Fatalf("tail len %d exceeds the cap %d", len(tail), maxRunLines)
	}
	if !strings.Contains(joined, "FINAL 27/u jwks") {
		t.Fatalf("tail lost the LAST line of the run: %q", joined)
	}
}

// A job that retries keeps one record per attempt, each with its own output.
func TestEdge_RetriedJobRecordsEachAttempt(t *testing.T) {
	l := newCapturing(t.TempDir())
	desired := []Service{{
		Name:    "migrate",
		Oneshot: true,
		Command: []string{"sh", "-c", "echo \"attempt at $(date +%s%N)\"; exit 1"},
	}}
	a, cancel, wg := startNewAgent(t, l, nil, desired, logSinkOpts())
	defer wg.Wait()
	defer cancel()

	waitFor(t, 10*time.Second, "several attempts recorded", func() bool { return len(a.JobRuns("migrate")) >= 2 })
	runs := a.JobRuns("migrate")
	for i, r := range runs {
		if len(r.LogTail) == 0 {
			t.Fatalf("attempt %d recorded with no output", i)
		}
		if !strings.Contains(strings.Join(r.LogTail, "\n"), "attempt at") {
			t.Fatalf("attempt %d tail = %v, want its own output", i, r.LogTail)
		}
	}
	// Distinct attempts must not share the identical tail (that was the bug).
	if len(runs) >= 2 {
		a0 := strings.Join(runs[0].LogTail, "\n")
		a1 := strings.Join(runs[1].LogTail, "\n")
		if a0 == a1 {
			t.Fatalf("two attempts share the identical tail %q; they cannot be told apart", a0)
		}
	}
}

// A job writing to stderr only (migrations often do) must still be captured.
func TestEdge_StderrOnlyJobIsCaptured(t *testing.T) {
	l := newCapturing(t.TempDir())
	desired := []Service{{
		Name:    "migrate",
		Oneshot: true,
		Command: []string{"sh", "-c", "echo 'error: dirty database version 27' >&2; exit 1"},
	}}
	a, cancel, wg := startNewAgent(t, l, nil, desired, logSinkOpts())
	defer wg.Wait()
	defer cancel()

	waitFor(t, 8*time.Second, "run recorded", func() bool { return len(a.JobRuns("migrate")) >= 1 })
	r := a.JobRuns("migrate")[0]
	if !strings.Contains(strings.Join(r.LogTail, "\n"), "dirty database version 27") {
		t.Fatalf("stderr-only output missing from the tail: %v", r.LogTail)
	}
}

// A job whose last line has no trailing newline must not lose that line.
func TestEdge_UnterminatedLastLineIsKept(t *testing.T) {
	l := newCapturing(t.TempDir())
	desired := []Service{{
		Name:    "migrate",
		Oneshot: true,
		Command: []string{"sh", "-c", "printf 'applied 27 no newline'; exit 0"},
	}}
	a, cancel, wg := startNewAgent(t, l, nil, desired, logSinkOpts())
	defer wg.Wait()
	defer cancel()

	waitFor(t, 5*time.Second, "run recorded", func() bool { return len(a.JobRuns("migrate")) == 1 })
	r := a.JobRuns("migrate")[0]
	if !strings.Contains(strings.Join(r.LogTail, "\n"), "applied 27 no newline") {
		t.Fatalf("unterminated final line lost: %v", r.LogTail)
	}
}
