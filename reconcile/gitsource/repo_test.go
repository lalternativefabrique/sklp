package gitsource

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// originRepo builds a real repository to clone from. git's own behaviour is
// what this package depends on — how it reports a missing path, what `show`
// accepts as a ref — so a stub would only assert what I assumed about it.
func originRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "--initial-branch=main")
	writeFile(t, dir, ".sklp/deploy-root.yaml", "kind: deploy-root\nnode: node-a\n")
	run("add", ".")
	run("commit", "-m", "first")
	return dir
}

func writeFile(t *testing.T, dir, path, body string) {
	t.Helper()
	full := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func commit(t *testing.T, dir, message string) {
	t.Helper()
	for _, args := range [][]string{{"add", "."}, {"commit", "-m", message}} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func TestFetchClonesThenReadsAFile(t *testing.T) {
	ctx := context.Background()
	r := New(originRepo(t), filepath.Join(t.TempDir(), "mirror.git"))

	head, err := r.Fetch(ctx, "main")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(head) != 40 {
		t.Fatalf("head = %q, want a full commit sha", head)
	}

	body, err := r.FetchFileAtRef(ctx, "", ".sklp/deploy-root.yaml", head, "")
	if err != nil {
		t.Fatalf("FetchFileAtRef: %v", err)
	}
	if !strings.Contains(body, "node-a") {
		t.Fatalf("body = %q, want the committed file", body)
	}
}

// A second Fetch must reuse the mirror and pick up new commits — the poll loop
// calls this on every tick, and re-cloning each time would make the interval
// unusable.
func TestFetchPicksUpNewCommits(t *testing.T) {
	ctx := context.Background()
	origin := originRepo(t)
	r := New(origin, filepath.Join(t.TempDir(), "mirror.git"))

	first, err := r.Fetch(ctx, "main")
	if err != nil {
		t.Fatalf("first Fetch: %v", err)
	}

	writeFile(t, origin, ".sklp/deploy-root.yaml", "kind: deploy-root\nnode: other-node\n")
	commit(t, origin, "second")

	second, err := r.Fetch(ctx, "main")
	if err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if second == first {
		t.Fatal("the second fetch returned the first commit; new work would never reconcile")
	}

	body, err := r.FetchFileAtRef(ctx, "", ".sklp/deploy-root.yaml", second, "")
	if err != nil {
		t.Fatalf("FetchFileAtRef: %v", err)
	}
	if !strings.Contains(body, "other-node") {
		t.Fatalf("body = %q, want the updated file", body)
	}
}

// The old ref must stay readable after a fetch: a reconcile pass resolves a
// commit and then reads several files at it, and a mirror that dropped history
// mid-pass would fail on the second read.
func TestOlderRefsStayReadable(t *testing.T) {
	ctx := context.Background()
	origin := originRepo(t)
	r := New(origin, filepath.Join(t.TempDir(), "mirror.git"))

	first, err := r.Fetch(ctx, "main")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	writeFile(t, origin, ".sklp/deploy-root.yaml", "kind: deploy-root\nnode: other-node\n")
	commit(t, origin, "second")
	if _, err := r.Fetch(ctx, "main"); err != nil {
		t.Fatalf("second Fetch: %v", err)
	}

	body, err := r.FetchFileAtRef(ctx, "", ".sklp/deploy-root.yaml", first, "")
	if err != nil {
		t.Fatalf("reading the older ref: %v", err)
	}
	if !strings.Contains(body, "node-a") {
		t.Fatalf("body = %q, want the file as it was at the first commit", body)
	}
}

// A repo with no deploy-root is "nothing to reconcile", which the handlers
// read off this error. Any other failure would abort the pass instead.
func TestMissingFileReportsErrFileNotFound(t *testing.T) {
	ctx := context.Background()
	r := New(originRepo(t), filepath.Join(t.TempDir(), "mirror.git"))

	head, err := r.Fetch(ctx, "main")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	_, err = r.FetchFileAtRef(ctx, "", ".sklp/absent.yaml", head, "")
	if !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("err = %v, want ErrFileNotFound", err)
	}
}

// A ref the mirror does not have reads the same way: not there to read.
func TestUnknownRefReportsErrFileNotFound(t *testing.T) {
	ctx := context.Background()
	r := New(originRepo(t), filepath.Join(t.TempDir(), "mirror.git"))
	if _, err := r.Fetch(ctx, "main"); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	_, err := r.FetchFileAtRef(ctx, "", ".sklp/deploy-root.yaml",
		"0000000000000000000000000000000000000000", "")
	if !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("err = %v, want ErrFileNotFound", err)
	}
}

// A branch the remote does not carry is an error, not an empty reconcile: a
// node configured for the wrong branch must say so rather than silently
// deploying nothing forever.
func TestFetchFailsOnAnUnknownBranch(t *testing.T) {
	r := New(originRepo(t), filepath.Join(t.TempDir(), "mirror.git"))

	if _, err := r.Fetch(context.Background(), "does-not-exist"); err == nil {
		t.Fatal("fetching an absent branch must fail")
	}
}

// An interrupted first clone leaves a directory with no git repository in it.
// Treating that as "already cloned" would wedge every later fetch, so it is
// re-cloned instead.
func TestFetchRecoversFromAHalfWrittenClone(t *testing.T) {
	ctx := context.Background()
	mirror := filepath.Join(t.TempDir(), "mirror.git")
	if err := os.MkdirAll(mirror, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(mirror, "leftover"), []byte("junk"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	r := New(originRepo(t), mirror)
	if _, err := r.Fetch(ctx, "main"); err != nil {
		t.Fatalf("Fetch over a half-written clone: %v", err)
	}
}

// A clone URL can carry a token. Git echoes the URL in its errors, so the
// message must not carry the credential into the logs.
func TestErrorsDoNotLeakRemoteCredentials(t *testing.T) {
	r := New("https://user:s3cr3t-token@example.invalid/acme/widgets.git",
		filepath.Join(t.TempDir(), "mirror.git"))

	_, err := r.Fetch(context.Background(), "main")
	if err == nil {
		t.Fatal("cloning an unreachable remote must fail")
	}
	if strings.Contains(err.Error(), "s3cr3t-token") {
		t.Fatalf("the error carries the token: %v", err)
	}
}
