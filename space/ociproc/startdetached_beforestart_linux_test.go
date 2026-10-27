package ociproc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeRuncRecording(t *testing.T, callsPath string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "runc")
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> " + callsPath + "\n" +
		"pf=\"\"\n" +
		"while [ $# -gt 0 ]; do\n" +
		"  if [ \"$1\" = \"--pid-file\" ]; then pf=\"$2\"; fi\n" +
		"  shift\n" +
		"done\n" +
		"[ -n \"$pf\" ] && echo 4242 > \"$pf\"\n" +
		"exit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func runcVerbs(t *testing.T, callsPath string) []string {
	t.Helper()
	raw, err := os.ReadFile(callsPath)
	if err != nil {
		t.Fatal(err)
	}
	var verbs []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		f := strings.Fields(line)
		verbs = append(verbs, f[2])
	}
	return verbs
}

func TestStartDetachedRunsBeforeStartBetweenCreateAndStart(t *testing.T) {
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	bin := fakeRuncRecording(t, calls)

	var hookPID int
	var verbsAtHook []string
	pid, err := startDetached(context.Background(), bin, filepath.Join(dir, "state"), dir, "id", filepath.Join(dir, "service.log"), false, 0, "", func(p int) error {
		hookPID = p
		verbsAtHook = runcVerbs(t, calls)
		return nil
	})
	if err != nil {
		t.Fatalf("startDetached: %v", err)
	}
	if pid != 4242 || hookPID != 4242 {
		t.Fatalf("pid=%d hookPID=%d, want 4242", pid, hookPID)
	}
	if strings.Join(verbsAtHook, ",") != "create" {
		t.Fatalf("runc calls before the hook = %v, want only create", verbsAtHook)
	}
	if got := strings.Join(runcVerbs(t, calls), ","); got != "create,start" {
		t.Fatalf("runc calls = %s, want create,start", got)
	}
}

func TestStartDetachedHookErrorDeletesContainer(t *testing.T) {
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	bin := fakeRuncRecording(t, calls)

	_, err := startDetached(context.Background(), bin, filepath.Join(dir, "state"), dir, "id", filepath.Join(dir, "service.log"), false, 0, "", func(int) error {
		return errors.New("no cgroup")
	})
	if err == nil {
		t.Fatal("hook error ignored")
	}
	if got := strings.Join(runcVerbs(t, calls), ","); got != "create,delete" {
		t.Fatalf("runc calls = %s, want create,delete (never started)", got)
	}
}

func TestStartDetachedWithoutHookKeepsRunDetached(t *testing.T) {
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	bin := fakeRuncRecording(t, calls)
	if _, err := startDetached(context.Background(), bin, filepath.Join(dir, "state"), dir, "id", filepath.Join(dir, "service.log"), false, 0, "", nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(runcVerbs(t, calls), ","); got != "run" {
		t.Fatalf("runc calls = %s, want run", got)
	}
}
