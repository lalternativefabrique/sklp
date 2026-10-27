package pipeline

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A branch with no configured upstream must not produce the multi-line
// "There is no tracking information for the current branch" stderr
// dump from `git pull` — the runner should detect the missing upstream
// up-front and return silently.
func TestGitFastForward_NoUpstreamIsSilent(t *testing.T) {
	repo := initRepoWithBranchNoUpstream(t)

	stderr := captureStderr(t, func() {
		gitFastForward(repo)
	})

	if stderr != "" {
		t.Fatalf("expected no stderr output on a branch with no upstream, got:\n%s", stderr)
	}
}

func initRepoWithBranchNoUpstream(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	run("commit", "--allow-empty", "-q", "-m", "init")
	run("checkout", "-q", "-b", "feat/new")
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	return abs
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	done := make(chan struct{})
	var buf bytes.Buffer
	go func() {
		_, _ = io.Copy(&buf, r)
		close(done)
	}()
	defer func() {
		os.Stderr = orig
	}()
	fn()
	_ = w.Close()
	<-done
	return buf.String()
}
