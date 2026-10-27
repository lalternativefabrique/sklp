package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/internal/cli"
)

// findDeploySub returns a deploy sub-command by name (test helper).
func findDeploySub(t *testing.T, name string) *cli.Command {
	t.Helper()
	cmd := deployCommand()
	for i := range cmd.Sub {
		if cmd.Sub[i].Name == name {
			return &cmd.Sub[i]
		}
	}
	t.Fatalf("deploy has no %q sub-command", name)
	return nil
}

// TestDeployRegistersDelete asserts the canonical `delete` verb is wired into
// the deploy tree and that the removed `down` alias is gone for good.
func TestDeployRegistersDelete(t *testing.T) {
	del := findDeploySub(t, "delete")
	if del.Run == nil {
		t.Fatal("delete has no Run")
	}
	cmd := deployCommand()
	for i := range cmd.Sub {
		if cmd.Sub[i].Name == "down" {
			t.Fatal("deploy still registers the removed `down` sub-command")
		}
	}
}

// captureStderr runs fn with os.Stderr redirected to a pipe and returns what was
// written. It mirrors how the verbs print user-facing notices/errors to stderr.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, rerr := r.Read(buf)
			if n > 0 {
				b.Write(buf[:n])
			}
			if rerr != nil {
				break
			}
		}
		done <- b.String()
	}()
	fn()
	os.Stderr = orig
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

// runDeploySub parses argv against the sub-command's declared flags and invokes
// its Run, returning the exit code. The args resolve a non-existent stack so the
// teardown fails fast at file resolution — BEFORE any container op — which is
// enough to exercise the dispatch + deprecation-notice path without a runtime.
func runDeploySub(t *testing.T, sub *cli.Command, argv ...string) int {
	t.Helper()
	a, err := cli.ParseLeaf(*sub, argv)
	if err != nil {
		t.Fatalf("parse deploy %s %v: %v", sub.Name, argv, err)
	}
	return sub.Run(t.Context(), a)
}

// TestDeleteDispatchesTeardown: `delete` reaches runDownSubcommand (here failing
// on an unknown stack, exit 2) without emitting any deprecation notice.
func TestDeleteDispatchesTeardown(t *testing.T) {
	repo := t.TempDir() // empty: no .sklp/<stack>/deploy.yaml -> resolve fails

	// Point the config loader at a file that does not exist. Without this the
	// deploy flags fall back to the DEVELOPER's active context (kubeconfig
	// model), netFlags.remote() reports true, and `delete` short-circuits into
	// runDownRemote — which returns 0 after asking that supervisor to stop.
	// The test then failed on a machine with a configured context and passed
	// in CI, where none exists.
	t.Setenv("SKLP_CONFIG", filepath.Join(repo, "absent.yaml"))

	del := findDeploySub(t, "delete")
	delErr := captureStderr(t, func() {
		if code := runDeploySub(t, del, "nope", "--repo", repo); code == 0 {
			t.Fatalf("delete on unknown stack: got exit 0, want non-zero")
		}
	})
	if strings.Contains(delErr, "deprecated") {
		t.Fatalf("delete must NOT print a deprecation notice; stderr=%q", delErr)
	}
}
