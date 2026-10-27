//go:build linux

package ociproc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// waitForContent polls path until it contains want or a short deadline elapses.
// The drain goroutine that copies the pipe into service.log outlives
// startDetached's return, so a one-shot read can race ahead of the copy.
func waitForContent(t *testing.T, path, want string) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && strings.Contains(string(b), want) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func readAll(t *testing.T, path string) string {
	t.Helper()
	b, _ := os.ReadFile(path)
	return string(b)
}

// fileUID returns the owning uid of path via the raw stat syscall.
func fileUID(t *testing.T, path string) uint32 {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return st.Uid
}

// fakeRuncEmitting writes a stub runc that, for `run -d`, writes the init PID to
// the --pid-file, emits a marker on stdout, records the type of fd 1 (the stdio
// it was handed), and exits 0. It lets us assert what the CONTAINER sees as its
// stdout without a real runc/kernel.
func fakeRuncEmitting(t *testing.T, marker, fdTypePath string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-runc")
	// `run -d` argv ends in: ... --pid-file <path> <id>. Pull the pid-file out and
	// write a pid. `stat -L -c %F /proc/self/fd/1` reports "fifo" for a pipe vs
	// "regular file" for a plain file — that is the invariant under test.
	// CRUCIAL: capture fd1's type into a shell variable FIRST, before any
	// redirection touches fd1. Redirecting the stat's output (`>file` or `>&9`)
	// re-points fd1 for that command, so it would inspect the wrong fd. Compute
	// into $t while fd1 is still the inherited stdio, THEN write $t out.
	script := "#!/bin/sh\n" +
		"exec 8>&1\n" + // dup the inherited stdio to fd8 BEFORE any cmd-subst/redir
		"t=$(stat -L -c %F /proc/self/fd/8 2>/dev/null)\n" + // inspect fd8, immune to $(...) pipe
		"echo \"$t\" > " + fdTypePath + "\n" +
		"echo \"" + marker + "\"\n" + // goes to whatever fd1 we were handed
		"pf=\"\"\n" +
		"while [ $# -gt 0 ]; do\n" +
		"  if [ \"$1\" = \"--pid-file\" ]; then pf=\"$2\"; fi\n" +
		"  shift\n" +
		"done\n" +
		"[ -n \"$pf\" ] && echo 4242 > \"$pf\"\n" +
		"exit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake runc: %v", err)
	}
	return bin
}

// TestStartDetachedPipesStdio proves the container's stdout is a PIPE owned by
// the supervisor, not a direct fd onto the host service.log. This is the fix for
// the rootless-log-ownership bug: an entrypoint that runs as container-root and
// chowns its files before dropping privileges (mongo's docker-entrypoint.sh →
// `gosu mongodb`) would re-own a directly-shared log file to a mapped subuid,
// breaking `deploy logs` and the `log:` readiness probe. With a pipe the
// supervisor owns service.log end to end; the container can never touch it.
func TestStartDetachedPipesStdio(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "service.log")
	fdTypePath := filepath.Join(dir, "fd1.type")
	bin := fakeRuncEmitting(t, "HELLO_FROM_CONTAINER", fdTypePath)

	pid, err := startDetached(context.Background(), bin, filepath.Join(dir, "state"), dir, "id", logPath, false, 0, "", nil)
	if err != nil {
		t.Fatalf("startDetached: %v", err)
	}
	if pid != 4242 {
		t.Fatalf("pid = %d, want 4242 (pid-file not read back)", pid)
	}

	// 1. The container's fd 1 must be a pipe, not a regular file: that is what
	//    severs the chown path onto service.log.
	fdType, err := os.ReadFile(fdTypePath)
	if err != nil {
		t.Fatalf("fake runc never recorded its fd1 type: %v", err)
	}
	if got := strings.TrimSpace(string(fdType)); got != "fifo" {
		t.Fatalf("container stdout fd type = %q, want \"fifo\" (a pipe); a regular file means the container can chown the host log", got)
	}

	// 2. The supervisor must still capture the container's output: the drain
	//    goroutine copies pipe → service.log. Poll briefly since the copy races
	//    the test (the goroutine outlives startDetached's return).
	if !waitForContent(t, logPath, "HELLO_FROM_CONTAINER") {
		t.Fatalf("service.log never received container stdout; got %q", readAll(t, logPath))
	}

	// 3. service.log is owned by the supervisor (the test process), never a
	//    mapped subuid — because the container only ever held a pipe end.
	if uid := fileUID(t, logPath); uid != uint32(os.Getuid()) {
		t.Fatalf("service.log uid = %d, want %d (supervisor); the container re-owned the log", uid, os.Getuid())
	}
}
