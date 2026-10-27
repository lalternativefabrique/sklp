package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// ParseArgsAndRun must return errors instead of calling os.Exit/log.Fatal:
// an invalid -resume + -no-cache combo proves the no-Fatal contract.
func TestParseArgsAndRun_InvalidComboReturnsError(t *testing.T) {
	_, err := ParseArgsAndRun(context.Background(), []string{
		"-resume", "-no-cache",
		"-task", "x.yaml", "-repo", t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected an error for -resume + -no-cache, got nil")
	}
	if !strings.Contains(err.Error(), "resume") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Missing -task / -repo returns an error, not a process exit.
func TestParseArgsAndRun_MissingRequiredFlags(t *testing.T) {
	_, err := ParseArgsAndRun(context.Background(), []string{"-no-pull"})
	if err == nil {
		t.Fatal("expected an error when -task and -repo are absent")
	}
}

// ExitCode maps nil→0, a plain error→1, and an *exitCodeError→its code.
func TestExitCode(t *testing.T) {
	if got := ExitCode(nil); got != 0 {
		t.Fatalf("nil: want 0, got %d", got)
	}
	if got := ExitCode(errors.New("boom")); got != 1 {
		t.Fatalf("plain: want 1, got %d", got)
	}
	if got := ExitCode(&exitCodeError{code: 7}); got != 7 {
		t.Fatalf("exitCodeError: want 7, got %d", got)
	}
}
