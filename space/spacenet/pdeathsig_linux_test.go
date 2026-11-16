//go:build linux

package spacenet

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func osThreadCount(t *testing.T) int {
	t.Helper()
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "Threads:"); ok {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				t.Fatal(err)
			}
			return n
		}
	}
	t.Fatal("no Threads line in /proc/self/status")
	return 0
}

func TestStartPinnedReleasesThreadAfterChildExit(t *testing.T) {
	const children = 50
	before := osThreadCount(t)
	for range children {
		cmd := exec.Command("true")
		cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
		if err := startPinned(cmd); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		after := osThreadCount(t)
		if after-before < children/2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("threads grew from %d to %d after %d exited children", before, after, children)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestStartPinnedKeepsLiveChildAcrossThreadChurn(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := startPinned(cmd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	done := make(chan struct{})
	for range 20 {
		go func() {
			defer func() { done <- struct{}{} }()
			c := exec.Command("true")
			c.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
			if err := startPinned(c); err == nil {
				_ = c.Wait()
			}
		}()
	}
	for range 20 {
		<-done
	}
	time.Sleep(200 * time.Millisecond)
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(cmd.Process.Pid) + "/stat")
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
	if fields[0] == "Z" {
		t.Fatal("live child was killed by thread churn")
	}
}
