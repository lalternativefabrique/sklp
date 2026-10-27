package pipeline

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// fakeTool writes an executable that prints body on stdout and exits 0.
func fakeTool(t *testing.T, name, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stubs are POSIX-only")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	script := "#!/bin/sh\n" + body + "\nexit 0\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// A CI node carried a `go` whose GOROOT was gone: it exited 0, printed only its
// complaint, and was taken for the host toolchain. Every later `go` call then
// failed far from where that choice was made.
func TestProbeHostVersionRejectsABinaryThatPrintsNoVersion(t *testing.T) {
	p := fakeTool(t, "go", `echo "go: cannot find GOROOT directory: 'go' binary is trimmed and GOROOT is not set"`)

	if _, ok := probeHostVersion(context.Background(), "go", p); ok {
		t.Fatal("a binary printing no version must not pass as the host toolchain")
	}
}

func TestProbeHostVersionAcceptsARealVersion(t *testing.T) {
	p := fakeTool(t, "go", `echo "go version go1.26.6 linux/amd64"`)

	got, ok := probeHostVersion(context.Background(), "go", p)
	if !ok {
		t.Fatal("a working tool must still be preferred")
	}
	if got != "1.26.6" {
		t.Fatalf("got %q, want 1.26.6", got)
	}
}

// The rejection must reach the caller: hostBinaryMatches is what the resolver
// asks, and a false there is what sends it on to install a real one.
func TestHostBinaryMatchesRejectsABrokenTool(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "go")
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho broken\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	if _, ok := hostBinaryMatches(context.Background(), "go", "1.26.6"); ok {
		t.Fatal("hostBinaryMatches accepted a tool that reports no version")
	}
}

// The runner image's tools farm is a symlink into its own tools cache
// (/opt/sklp/tools/go/<ver>/…). Once a build moves into another rootfs that
// target is gone, and taking the link as the host toolchain sent every `go`
// call to a dead link, surfacing far away as "cannot find GOROOT directory".
//
// This Go's LookPath rejects a dangling link on its own, so the link is made
// dangling after LookPath has already accepted it — which is also the real
// sequence: the probe resolves a live path, and the rootfs it points into is
// swapped out from under it.
//
// It covers the outcome, not one guard: hostBinaryMatches stats the path and
// then runs it, and a dead link fails both. Removing the stat alone keeps
// this passing, which is the point — the tool must not be accepted, by
// whichever check gets there first.
func TestHostBinaryMatchesRejectsABrokenSymlink(t *testing.T) {
	// A name no machine has installed: PATH is set to the test's directory,
	// but a tool the runner image also ships would still be found there
	// first on some layouts, and the link under test would never be reached.
	const tool = "sklp-probe-tool"
	dir := t.TempDir()
	target := filepath.Join(dir, tool+"-real")
	if err := os.WriteFile(target, []byte("#!/bin/sh\necho v22.14.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, tool)
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	t.Setenv("PATH", dir)
	// Guard the premise: the link must be what LookPath resolves to, or the
	// test proves nothing about the case it is meant to cover.
	if got, err := exec.LookPath(tool); err != nil || got != link {
		t.Skipf("LookPath resolved %q (%v), want %q", got, err, link)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}

	if _, ok := hostBinaryMatches(context.Background(), tool, "22.14.0"); ok {
		t.Fatal("a dangling symlink must not pass as the host toolchain")
	}
}

func TestHostGoWithoutItsGorootIsRejected(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "go")
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  version) echo 'go version go1.26.6 linux/amd64' ;;\n" +
		"  env) echo '/nonexistent/goroot' ;;\n" +
		"esac\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	if hostToolIsUsable(context.Background(), "go", stub) {
		t.Fatal("a go whose GOROOT does not exist must not be taken as the host toolchain")
	}
}

func TestHostGoWithARealGorootIsAccepted(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "go")
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  version) echo 'go version go1.26.6 linux/amd64' ;;\n" +
		"  env) echo '" + dir + "' ;;\n" +
		"esac\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	if !hostToolIsUsable(context.Background(), "go", stub) {
		t.Fatal("a working toolchain must still be usable, or every run re-downloads go")
	}
}
