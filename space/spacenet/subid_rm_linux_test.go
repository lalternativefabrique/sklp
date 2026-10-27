//go:build linux

package spacenet

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/lalternativefabrique/sklp/internal/integtest"
)

// TestRemoveAllAsRoot_FastPath removes an operator-owned tree through the plain
// os.RemoveAll branch — no fork, no newuidmap needed. This is the common case
// (most volumes are operator-owned) and must never escalate.
func TestRemoveAllAsRoot_FastPath(t *testing.T) {
	dir := t.TempDir()
	tree := filepath.Join(dir, "vol")
	if err := os.MkdirAll(filepath.Join(tree, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "sub", "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAllAsRoot(tree); err != nil {
		t.Fatalf("RemoveAllAsRoot operator-owned tree: %v", err)
	}
	if _, err := os.Stat(tree); !os.IsNotExist(err) {
		t.Fatalf("tree survived removal: %v", err)
	}
}

// seedSubidEnv marks a re-exec of the test binary that creates a tree as uid 0
// inside the mapped subuid range (so the files land owned by a host subuid the
// operator cannot delete directly), then exits. Same userns handshake as the
// remove child. The path to seed is passed via envRmPath.
const seedSubidEnv = "SKLP_TEST_SEED_SUBID"

func maybeRunSeedSubid() {
	switch os.Getenv(seedSubidEnv) {
	case "1":
		// Stage 1: no caps yet; re-exec into stage 2 once the maps are written.
		// Strip the old marker + sync fd so stage 2 does not loop back into stage 1.
		waitSubidMapping()
		env := make([]string, 0, len(os.Environ()))
		for _, kv := range os.Environ() {
			if strings.HasPrefix(kv, seedSubidEnv+"=") || strings.HasPrefix(kv, envSyncFD+"=") {
				continue
			}
			env = append(env, kv)
		}
		env = append(env, seedSubidEnv+"=2")
		_ = syscall.Exec(os.Args[0], os.Args, env)
		os.Exit(1)
	case "2":
		// fall through to the seeding logic below
	default:
		return
	}
	tree := os.Getenv(envRmPath)
	// Create the parent as the userns root (host operator) so the operator can
	// traverse it, but chown the inner tree to a non-zero userns uid so the FILES
	// land owned by a host subuid (100000+998) the operator cannot delete.
	inner := filepath.Join(tree, "sub")
	if err := os.MkdirAll(inner, 0o700); err != nil {
		os.Exit(1)
	}
	if err := os.WriteFile(filepath.Join(inner, "f"), []byte("x"), 0o600); err != nil {
		os.Exit(1)
	}
	if err := os.Chown(filepath.Join(inner, "f"), 998, 998); err != nil {
		os.Exit(2)
	}
	if err := os.Chown(inner, 998, 998); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

// TestRemoveAllAsRoot_Userns removes a subuid-owned tree via the userns
// escalation. It seeds the tree as uid 0 inside a mapped userns (so the files
// are owned by a host subuid the operator cannot delete), then asserts
// RemoveAllAsRoot reaps it. Needs newuidmap/newgidmap + a configured subuid
// range; it skips otherwise (Demand fails the skip on a known-capable runner).
func TestRemoveAllAsRoot_Userns(t *testing.T) {
	tools, ok := lookupSubidTools()
	if !ok {
		integtest.Demand(t, "newuidmap/newgidmap missing")
		t.Skip("newuidmap/newgidmap missing; cannot map subuid range")
	}
	dir := t.TempDir()
	tree := filepath.Join(dir, "vol")

	if err := seedSubidTree(tools, tree); err != nil {
		integtest.Demand(t, "subuid range seeding failed (no configured /etc/subuid?)")
		t.Skipf("seed subuid tree: %v", err)
	}
	// The operator must NOT be able to delete it directly (it is subuid-owned).
	if os.RemoveAll(tree) == nil {
		t.Skip("seeded tree was operator-deletable; subuid range not effective here")
	}
	if err := RemoveAllAsRoot(tree); err != nil {
		t.Fatalf("RemoveAllAsRoot subuid tree: %v", err)
	}
	if _, err := os.Stat(tree); !os.IsNotExist(err) {
		t.Fatalf("subuid tree survived removal: %v", err)
	}
}

// seedSubidTree forks the test binary into a mapped userns and has it create
// tree as uid 0, landing the files under a host subuid. Mirrors removeInUserns.
func seedSubidTree(tools subidTools, tree string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(), seedSubidEnv+"=1", envRmPath+"="+tree,
		fmt.Sprintf("%s=%d", envSyncFD, 3))
	cmd.ExtraFiles = []*os.File{pr}
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWUSER}
	if err := startPinned(cmd); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		return err
	}
	_ = pr.Close()
	id, err := resolveIdentity(os.Getuid(), os.Getgid())
	if err != nil {
		_ = pw.Close()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		return err
	}
	mapErr := tools.applySubidRange(cmd.Process.Pid, id)
	_ = pw.Close()
	if mapErr != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		return mapErr
	}
	return cmd.Wait()
}
