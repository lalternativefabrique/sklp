package pipeline

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLineTail_KeepsLastN(t *testing.T) {
	tail := newLineTail(3)
	for _, ln := range []string{"a\n", "b\n", "c\n", "d\n", "e\n"} {
		if _, err := tail.Write([]byte(ln)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	got := tail.String()
	want := "c\nd\ne"
	if got != want {
		t.Errorf("tail = %q, want %q", got, want)
	}
}

func TestLineTail_KeepsPartialTrailingLine(t *testing.T) {
	tail := newLineTail(2)
	tail.Write([]byte("a\nb\npartial"))
	got := tail.String()
	want := "b\npartial"
	if got != want {
		t.Errorf("tail = %q, want %q", got, want)
	}
}

func TestLineTail_HandlesChunkedWrites(t *testing.T) {
	tail := newLineTail(4)
	tail.Write([]byte("hel"))
	tail.Write([]byte("lo\nwor"))
	tail.Write([]byte("ld\n"))
	got := tail.String()
	want := "hello\nworld"
	if got != want {
		t.Errorf("tail = %q, want %q", got, want)
	}
}

func TestFormatAutoInstallFailure_WithLogPath(t *testing.T) {
	f := &autoInstallFailure{
		Tool:    "pnpm",
		Cmdline: "pnpm install --frozen-lockfile",
		Tail:    "\n\nERR_PNPM_LOCKFILE_CONFIG_MISMATCH\nupdate your lockfile\n",
	}
	got := formatAutoInstallFailure(f, "/tmp/ci.log")
	if !strings.Contains(got, "pnpm install --frozen-lockfile (last 2 lines):") {
		t.Errorf("missing header in %q", got)
	}
	if !strings.Contains(got, "  ERR_PNPM_LOCKFILE_CONFIG_MISMATCH") {
		t.Errorf("missing indented body in %q", got)
	}
	if !strings.HasSuffix(got, "full output: /tmp/ci.log") {
		t.Errorf("missing log pointer in %q", got)
	}
}

func TestFormatAutoInstallFailure_OmitsLogPointerWhenEmpty(t *testing.T) {
	f := &autoInstallFailure{Tool: "go", Cmdline: "go mod download", Tail: "boom\n"}
	got := formatAutoInstallFailure(f, "")
	if strings.Contains(got, "full output") {
		t.Errorf("should not mention log path when empty: %q", got)
	}
	if !strings.Contains(got, "  boom") {
		t.Errorf("missing body in %q", got)
	}
}

func TestFormatAutoInstallFailure_Nil(t *testing.T) {
	if got := formatAutoInstallFailure(nil, "x"); got != "" {
		t.Errorf("nil failure should render empty, got %q", got)
	}
}

// TestEnsureDeps_CapturesTailOnFailure exercises the real ensureDeps
// path against a failing auto-install. We swap autoInstallCommand's
// effect by writing a fake `pnpm` shim onto PATH that prints a recognisable
// error to stderr and exits 1, mirroring the issue's reproducer.
func TestEnsureDeps_CapturesTailOnFailure(t *testing.T) {
	repo := t.TempDir()
	// Pretend this is a pnpm repo so hasManifest("pnpm") returns true.
	if err := os.WriteFile(filepath.Join(repo, "package.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Fake pnpm shim. ensureDeps prepends binDir to PATH, so dropping the
	// shim into binDir is enough.
	binDir := t.TempDir()
	shim := filepath.Join(binDir, "pnpm")
	script := "#!/bin/sh\n" +
		"echo 'Scope: all 7 workspace projects'\n" +
		"echo 'ERR_PNPM_LOCKFILE_CONFIG_MISMATCH  Cannot proceed with the frozen installation.' >&2\n" +
		"echo 'Update your lockfile using \"pnpm install --no-frozen-lockfile\"' >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	r := newToolResolver(map[string]string{"pnpm": "10.0.0"}, t.TempDir(), repo, io.Discard)
	err := r.ensureDeps(context.Background(), "pnpm", binDir)
	if err == nil {
		t.Fatal("expected ensureDeps to fail")
	}
	if !strings.Contains(err.Error(), "auto-install pnpm") {
		t.Errorf("wrapper error lost: %v", err)
	}

	fail := r.ConsumeLastAutoInstallFailure()
	if fail == nil {
		t.Fatal("expected lastFail to be populated")
	}
	if fail.Tool != "pnpm" {
		t.Errorf("Tool = %q, want pnpm", fail.Tool)
	}
	if fail.Cmdline != "pnpm install --frozen-lockfile" {
		t.Errorf("Cmdline = %q", fail.Cmdline)
	}
	if !strings.Contains(fail.Tail, "ERR_PNPM_LOCKFILE_CONFIG_MISMATCH") {
		t.Errorf("Tail missing the underlying error: %q", fail.Tail)
	}

	// Second consume returns nil — the slot is cleared so an unrelated
	// later failure does not reuse stale context.
	if again := r.ConsumeLastAutoInstallFailure(); again != nil {
		t.Errorf("expected second consume to return nil, got %+v", again)
	}
}
