package devstack

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureStd runs fn with os.Stdout/os.Stderr redirected to pipes and
// returns what was written to each. Main writes to the real os streams,
// so the test must swap them to observe the catalogue / error output.
func captureStd(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	origOut, origErr := os.Stdout, os.Stderr
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	os.Stdout, os.Stderr = outW, errW
	defer func() { os.Stdout, os.Stderr = origOut, origErr }()

	fn()

	_ = outW.Close()
	_ = errW.Close()
	ob, _ := io.ReadAll(outR)
	eb, _ := io.ReadAll(errR)
	return string(ob), string(eb)
}

// writeDevStack drops a minimal valid devstack YAML at .sklp/<stack>/dev.yaml.
func writeDevStack(t *testing.T, repo, stack string) {
	t.Helper()
	dir := filepath.Join(repo, ".sklp", stack)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := "version: 1\nkind: devstack\nservices:\n  api: { run: \"echo a\" }\n"
	if err := os.WriteFile(filepath.Join(dir, "dev.yaml"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

// `sklp dev` with no stack lists the dev-capable stacks and exits 0.
func TestDevMain_NoStack_ListsStacks(t *testing.T) {
	repo := t.TempDir()
	writeDevStack(t, repo, "core")
	writeDevStack(t, repo, "transcript")

	var code int
	out, _ := captureStd(t, func() { code = Main([]string{"-repo", repo}) })
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, want := range []string{"Available dev stacks", "core", "transcript"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q:\n%s", want, out)
		}
	}
}

// `sklp dev` with no stack and an empty .sklp/ prints the helpful hint.
func TestDevMain_NoStack_Empty(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".sklp"), 0o755); err != nil {
		t.Fatal(err)
	}
	var code int
	out, _ := captureStd(t, func() { code = Main([]string{"-repo", repo}) })
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "no dev stacks") {
		t.Fatalf("stdout missing the empty hint:\n%s", out)
	}
}

// `sklp dev <known>` resolves .sklp/<stack>/dev.yaml; -validate parses it
// and returns 0 without launching anything.
func TestDevMain_KnownStack_Validate(t *testing.T) {
	repo := t.TempDir()
	writeDevStack(t, repo, "core")

	var code int
	_, _ = captureStd(t, func() { code = Main([]string{"-repo", repo, "-validate", "core"}) })
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (validate should not launch)", code)
	}
}

// Regression: a flag placed AFTER the positional <stack> must still be honored.
// Go's flag.Parse stops at the first non-flag, so `sklp dev core -validate`
// once silently dropped -validate and launched the stack for real. The output
// must be the validate summary, not a launch.
func TestDevMain_FlagAfterStack(t *testing.T) {
	repo := t.TempDir()
	writeDevStack(t, repo, "core")

	var code int
	out, _ := captureStd(t, func() { code = Main([]string{"-repo", repo, "core", "-validate"}) })
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "service(s)") || strings.Contains(out, "starting") {
		t.Fatalf("flag after stack dropped — expected validate summary, got a launch:\n%s", out)
	}
}

// `sklp dev <unknown>` exits 2 and suggests the known stacks.
func TestDevMain_UnknownStack(t *testing.T) {
	repo := t.TempDir()
	writeDevStack(t, repo, "core")

	var code int
	_, errOut := captureStd(t, func() { code = Main([]string{"-repo", repo, "nope"}) })
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errOut, "unknown stack \"nope\"") {
		t.Fatalf("stderr missing unknown-stack error:\n%s", errOut)
	}
	if !strings.Contains(errOut, "core") {
		t.Fatalf("stderr missing the suggestion list:\n%s", errOut)
	}
}
