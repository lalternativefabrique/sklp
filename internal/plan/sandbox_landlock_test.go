//go:build linux

package plan

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"

	llsyscall "github.com/landlock-lsm/go-landlock/landlock/syscall"
)

// landlockChildSkip is the exit code the child uses to signal "Landlock
// is not enforced on this kernel" so the parent can t.Skip instead of
// failing. Any 1 means a real assertion mismatch.
const landlockChildSkip = 99

// TestRestrictLandlock_DeniesHomeAllowsWorkdir is the real-kernel
// integration test: it applies the confined policy to a subprocess and
// asserts the granted workdir is readable while a path outside every
// grant (the simulated $HOME secret) is denied with EACCES.
//
// It runs in a re-exec'd subprocess because Landlock self-restricts the
// whole process irreversibly — applying it (or even probing support via a
// real RestrictPaths) in the test runner would poison every later test
// with a read-only "/". So kernel-support detection happens in the CHILD,
// which reports "unsupported" via exit code landlockChildSkip; the parent
// never touches Landlock and skips on that code.
func TestRestrictLandlock_DeniesHomeAllowsWorkdir(t *testing.T) {
	if os.Getenv("SKLP_LANDLOCK_CHILD") == "1" {
		runLandlockChild()
		return
	}

	// Two sibling dirs: one granted rw (the "repo"), one ungranted (the
	// "home secret"). The child must read the first and fail the second.
	// os.MkdirTemp (not t.TempDir) so the test also runs under harnesses
	// that restrict the testing framework's default temp root.
	base, err := os.MkdirTemp("", "sklp-landlock-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	repo := filepath.Join(base, "repo")
	home := filepath.Join(base, "home")
	for _, d := range []string{repo, home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	repoFile := filepath.Join(repo, "ok.txt")
	homeFile := filepath.Join(home, "secret.txt")
	gitConfig := filepath.Join(home, ".gitconfig") // a FILE, granted via rofile
	if err := os.WriteFile(repoFile, []byte("repo"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(homeFile, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gitConfig, []byte("[user]\n\tname = t\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Re-exec only this test in a subprocess, carrying the policy inputs
	// through the environment.
	cmd := exec.Command(os.Args[0], "-test.run=^"+regexp.QuoteMeta(t.Name())+"$", "-test.v")
	cmd.Env = append(os.Environ(),
		"SKLP_LANDLOCK_CHILD=1",
		"SKLP_LL_REPO="+repoFile,
		"SKLP_LL_HOME="+homeFile,
		"SKLP_LL_GRANT="+repo,
		"SKLP_LL_GITCONFIG="+gitConfig,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == landlockChildSkip {
			t.Skip("Landlock not enforced on this kernel; skipping real-confinement test")
		}
		t.Fatalf("subprocess failed: %v\n%s", err, out)
	}
}

// runLandlockChild applies a confined policy granting only SKLP_LL_GRANT
// (plus the system roots needed to run), then verifies the grant is
// readable and the home path is denied. Exits non-zero on any mismatch
// so the parent's subprocess call fails.
func runLandlockChild() {
	// Detect kernel support with a NON-destructive ABI query (it does not
	// restrict anything), so the child's own real policy below is the only
	// Landlock state applied. A strict RestrictPaths probe would lock the
	// child read-only and break its own assertions.
	if v, err := llsyscall.LandlockGetABIVersion(); err != nil || v < 1 {
		os.Exit(landlockChildSkip)
	}
	grant := os.Getenv("SKLP_LL_GRANT")
	gitcfg := os.Getenv("SKLP_LL_GITCONFIG") // a regular FILE granted via rofile
	policy := []fsAccess{
		{Kind: "rw", Paths: []string{grant}},
		{Kind: "ro", Paths: defaultRORoots},
		{Kind: "rofile", Paths: []string{gitcfg}},
		{Kind: "rwfile", Paths: rwDevices},
	}
	if err := restrictLandlock(policy); err != nil {
		// A rofile grant on a regular file must NOT trip "inconsistent
		// access rights" — that was the gitconfig bug that silently
		// degraded the whole sandbox. Failing here proves regression.
		fmt.Fprintf(os.Stderr, "landlock child: restrict: %v\n", err)
		os.Exit(1)
	}
	// The rofile-granted gitconfig must be readable after confinement.
	if gitcfg != "" {
		if _, err := os.ReadFile(gitcfg); err != nil {
			fmt.Fprintf(os.Stderr, "landlock child: rofile gitconfig unreadable: %v\n", err)
			os.Exit(1)
		}
	}
	if _, err := os.ReadFile(os.Getenv("SKLP_LL_REPO")); err != nil {
		fmt.Fprintf(os.Stderr, "landlock child: granted repo file unreadable: %v\n", err)
		os.Exit(1)
	}
	if _, err := os.ReadFile(os.Getenv("SKLP_LL_HOME")); err == nil {
		fmt.Fprintln(os.Stderr, "landlock child: ungranted home file was readable — confinement leaked")
		os.Exit(1)
	} else if !os.IsPermission(err) {
		fmt.Fprintf(os.Stderr, "landlock child: home read failed with non-permission error: %v\n", err)
		os.Exit(1)
	}
	// The rwfile grant must let a process open /dev/null read-write —
	// the exact access swag/git need ("could not open /dev/null for
	// reading and writing"). Without the per-file grant the ro /dev
	// rule denies the write.
	if f, err := os.OpenFile("/dev/null", os.O_RDWR, 0); err != nil {
		fmt.Fprintf(os.Stderr, "landlock child: /dev/null not writable under rwfile grant: %v\n", err)
		os.Exit(1)
	} else {
		_ = f.Close()
	}
}
