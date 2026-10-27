//go:build linux

package spacenet

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// writeSingleIDMaps maps only the parent's own ids onto container id 0, which
// any unprivileged process may write itself: no setuid helper involved.
func writeSingleIDMaps(pid int) error {
	proc := fmt.Sprintf("/proc/%d/", pid)
	if err := os.WriteFile(proc+"setgroups", []byte("deny"), 0); err != nil {
		return err
	}
	if err := os.WriteFile(proc+"gid_map", []byte(fmt.Sprintf("0 %d 1\n", os.Getgid())), 0); err != nil {
		return err
	}
	return os.WriteFile(proc+"uid_map", []byte(fmt.Sprintf("0 %d 1\n", os.Getuid())), 0)
}

func TestStartMapped_ChildRunsOnlyOnceMapped(t *testing.T) {
	cmd := exec.Command("sh", "-c", "cat <&3 >/dev/null; cat /proc/self/uid_map")
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	cmd.ExtraFiles = nil

	if err := startMapped(cmd, writeSingleIDMaps); err != nil {
		t.Fatalf("startMapped: %v (%s)", err, errb.String())
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("child: %v (%s)", err, errb.String())
	}
	want := fmt.Sprintf("0 %d 1", os.Getuid())
	if got := strings.Join(strings.Fields(out.String()), " "); got != want {
		t.Fatalf("child uid_map = %q, want %q", got, want)
	}
}

func TestStartMapped_PipeTakesTheSlotAfterCallerFiles(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	cmd := exec.Command("sh", "-c", "cat <&4 >/dev/null; echo released >&3")
	cmd.ExtraFiles = []*os.File{w}
	var errb bytes.Buffer
	cmd.Stderr = &errb

	if err := startMapped(cmd, writeSingleIDMaps); err != nil {
		t.Fatalf("startMapped: %v (%s)", err, errb.String())
	}
	_ = w.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("child: %v (%s)", err, errb.String())
	}
	got := make([]byte, 32)
	n, _ := r.Read(got)
	if strings.TrimSpace(string(got[:n])) != "released" {
		t.Fatalf("caller fd 3 got %q", got[:n])
	}
	if !strings.Contains(cmd.Env[len(cmd.Env)-1], envSyncFD+"=4") {
		t.Fatalf("sync fd env = %q, want fd 4", cmd.Env[len(cmd.Env)-1])
	}
}

func TestStartMapped_MapFailureKillsTheChild(t *testing.T) {
	cmd := exec.Command("sh", "-c", "cat <&3 >/dev/null; sleep 30")
	boom := fmt.Errorf("boom")
	err := startMapped(cmd, func(int) error { return boom })
	if err != boom {
		t.Fatalf("err = %v, want the map error", err)
	}
	if cmd.ProcessState == nil || cmd.ProcessState.Success() {
		t.Fatalf("child was not reaped as killed: %v", cmd.ProcessState)
	}
}
