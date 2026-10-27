package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// listTasks now delegates the pipelines walk to the layout package. It
// must still print the deterministic catalogue, scoped to the cwd's
// .sklp/pipelines, without mutating os.Args.
func TestRunCatalogue_ListsPipelinesViaLayout(t *testing.T) {
	repo := t.TempDir()
	dir := filepath.Join(repo, ".sklp", "pipelines")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"ci.yaml", "publish.yaml", "_template.yaml"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("kind: pipeline\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	chdir(t, repo)
	argsBefore := append([]string{}, os.Args...)

	r, w, _ := os.Pipe()
	origOut := os.Stdout
	os.Stdout = w
	listTasks(os.Stdout)
	_ = w.Close()
	os.Stdout = origOut
	b, _ := io.ReadAll(r)
	out := string(b)

	if !strings.Contains(out, "Available pipelines") {
		t.Fatalf("missing header:\n%s", out)
	}
	for _, want := range []string{"ci", "publish"} {
		if !strings.Contains(out, "  "+want+"\n") {
			t.Fatalf("missing pipeline %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "_template") {
		t.Fatalf("template should be hidden:\n%s", out)
	}
	if len(os.Args) != len(argsBefore) {
		t.Fatalf("os.Args was mutated: before=%v after=%v", argsBefore, os.Args)
	}
}

// chdir switches into dir for the duration of the test, restoring the
// prior cwd on cleanup. EvalSymlinks normalises /tmp on macOS/linux.
func chdir(t *testing.T, dir string) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
}
