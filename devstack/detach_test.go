package devstack

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestWithoutDetachFlag(t *testing.T) {
	got := withoutDetachFlag([]string{"-d", "messag", "--d", "-d=true", "-only", "core", "d", "-d=false"})
	want := []string{"messag", "-only", "core", "d", "-d=false"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestFollowUntilReady_StopsAtTheReadyBanner(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "dev.log")
	content := "[db] ready (0.4s)\n" + stackReadyBanner + " (embedded runc)\n[core] after ready\n"
	if err := os.WriteFile(logPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	outcome, _ := followUntilReady(logPath, &out, make(chan int), nil)
	if outcome != startupReady {
		t.Fatalf("outcome = %v, want ready", outcome)
	}
	if !strings.Contains(out.String(), "[db] ready") || strings.Contains(out.String(), "after ready") {
		t.Fatalf("followed output should stop at the banner:\n%s", out.String())
	}
}

func TestFollowUntilReady_ReportsAnExitBeforeReady(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "dev.log")
	if err := os.WriteFile(logPath, []byte("[core] ready probe failed\nno newline"), 0o644); err != nil {
		t.Fatal(err)
	}
	exited := make(chan int, 1)
	exited <- 3
	var out strings.Builder
	outcome, code := followUntilReady(logPath, &out, exited, nil)
	if outcome != startupExited || code != 3 {
		t.Fatalf("got (%v, %d), want (exited, 3)", outcome, code)
	}
	if !strings.Contains(out.String(), "no newline") {
		t.Fatalf("the unterminated last line must still be shown:\n%s", out.String())
	}
}
