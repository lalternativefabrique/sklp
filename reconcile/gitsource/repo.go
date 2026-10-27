// Package gitsource keeps a local mirror of the repos a node reconciles from,
// and reads files out of it at a given ref.
//
// ADR-014 has the node PULL rather than be pushed to: a webhook arrives at
// core, so reaching the node from there needs the node to be reachable, which
// is the one thing a reconciler installed on someone else's machine cannot
// assume. Polling inverts it — the node only needs to reach Git and the
// registries outbound, so a node behind NAT reconciles exactly like one with a
// public address.
//
// The price is latency: a push is reconciled within the poll interval instead
// of in seconds. That is the trade the ADR takes, and the interval is the knob.
package gitsource

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lalternativefabrique/sklp/reconcile/source"
)

// ErrFileNotFound aliases the contract's sentinel. Declaring a second one here
// would compile and never match: errors.Is compares identity, so a handler
// checking source.ErrFileNotFound would read "no image-watches.yaml" as a
// failed pass — which is exactly what it did until this alias replaced it.
var ErrFileNotFound = source.ErrFileNotFound

// Repo is a bare mirror of one remote, refreshed by Fetch and read by
// FetchFileAtRef.
//
// It is bare on purpose: nothing here ever needs a working tree, and a mirror
// avoids the checkout that would make two concurrent reads of different refs
// fight over the same files.
type Repo struct {
	// Remote is the clone URL, credentials included when the repo is private.
	Remote string
	// Dir is where the mirror lives on disk.
	Dir string
	// Timeout bounds every git invocation. A hung fetch must not wedge the
	// reconcile loop that called it.
	Timeout time.Duration

	mu sync.Mutex
}

// New prepares a Repo. The mirror itself is created on the first Fetch, so
// constructing one costs nothing and never touches the network.
func New(remote, dir string) *Repo {
	return &Repo{Remote: remote, Dir: dir, Timeout: 2 * time.Minute}
}

// Fetch brings the mirror up to date and returns the commit the default branch
// points at, which is what a reconcile pass runs against.
//
// It clones on first use and fetches after that. Both are done under a lock:
// the poller and an operator-triggered pass can arrive together, and git does
// not tolerate two writers in one directory.
func (r *Repo) Fetch(ctx context.Context, branch string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := r.ensureMirror(ctx); err != nil {
		return "", err
	}
	if _, err := r.git(ctx, "fetch", "--prune", "origin",
		fmt.Sprintf("+refs/heads/%s:refs/heads/%s", branch, branch)); err != nil {
		return "", fmt.Errorf("fetch %s: %w", branch, err)
	}
	head, err := r.git(ctx, "rev-parse", "refs/heads/"+branch)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", branch, err)
	}
	return strings.TrimSpace(head), nil
}

// FetchFileAtRef reads one file at a ref, satisfying the FileFetcher port the
// reconcile handlers declare. repo and token are ignored: a node mirrors one
// remote and authenticates when it fetches, not per file — the arguments stay
// so the same handler works against GitHub in core and a local mirror here.
func (r *Repo) FetchFileAtRef(ctx context.Context, _, path, ref, _ string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	out, err := r.git(ctx, "show", ref+":"+path)
	if err != nil {
		// git reports a missing path and a missing ref the same way on stderr;
		// both mean "not there to read", which is what the caller acts on.
		if strings.Contains(err.Error(), "does not exist") ||
			strings.Contains(err.Error(), "exists on disk, but not in") ||
			strings.Contains(err.Error(), "unknown revision") ||
			strings.Contains(err.Error(), "invalid object name") {
			return "", ErrFileNotFound
		}
		return "", err
	}
	return out, nil
}

// ensureMirror clones on first use. A directory that exists but holds no git
// repository is treated as absent and re-cloned: a half-written clone from an
// interrupted first run would otherwise wedge every later fetch.
func (r *Repo) ensureMirror(ctx context.Context) error {
	if _, err := os.Stat(filepath.Join(r.Dir, "HEAD")); err == nil {
		return nil
	}
	if err := os.RemoveAll(r.Dir); err != nil {
		return fmt.Errorf("clear %s: %w", r.Dir, err)
	}
	if err := os.MkdirAll(filepath.Dir(r.Dir), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(r.Dir), err)
	}
	cmdCtx, cancel := context.WithTimeout(ctx, r.timeout())
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, "git", "clone", "--bare", r.Remote, r.Dir)
	cmd.Env = gitEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		// The remote may carry a token; report the git message without it.
		return fmt.Errorf("clone: %s", redact(string(out), r.Remote))
	}
	return nil
}

func (r *Repo) git(ctx context.Context, args ...string) (string, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, r.timeout())
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, "git", append([]string{"-C", r.Dir}, args...)...)
	cmd.Env = gitEnv()
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", fmt.Errorf("git %s: %s", args[0], redact(strings.TrimSpace(string(exitErr.Stderr)), r.Remote))
		}
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return string(out), nil
}

func (r *Repo) timeout() time.Duration {
	if r.Timeout <= 0 {
		return 2 * time.Minute
	}
	return r.Timeout
}

// gitEnv keeps git from stopping to ask for anything. A reconcile loop has no
// terminal to answer a credential prompt on, so an unauthenticated fetch must
// fail rather than hang forever.
func gitEnv() []string {
	return append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"GCM_INTERACTIVE=never",
	)
}

// redact removes a remote's credentials from a git message, since a clone URL
// carrying a token would otherwise reach the logs verbatim.
func redact(msg, remote string) string {
	if remote == "" {
		return msg
	}
	msg = strings.ReplaceAll(msg, remote, "<remote>")
	if at := strings.LastIndex(remote, "@"); at > 0 {
		if scheme := strings.Index(remote, "://"); scheme >= 0 && scheme+3 < at {
			msg = strings.ReplaceAll(msg, remote[scheme+3:at], "<credentials>")
		}
	}
	return msg
}
