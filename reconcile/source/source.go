// Package source declares what a reconcile reads its declarations from.
//
// The handlers used to take that contract from the GitHub client itself: they
// compared errors against github.ErrFileNotFound, which meant importing 564
// lines of issues, pull requests and code search to reach one sentinel. On a
// node that reads a local mirror, none of that code has any business being in
// the binary.
//
// So the contract lives here, where both implementations can satisfy it — the
// GitHub client in core, the git mirror on the node — and neither side has to
// know the other exists.
package source

import (
	"context"
	"errors"
)

// ErrFileNotFound reports a path absent from the repo at that ref.
//
// It is distinct from a transport or auth failure on purpose: a repo with no
// deploy-root.yaml is "nothing to reconcile", which every handler treats as a
// normal outcome, while a network error means the answer is unknown and the
// pass must be retried rather than concluded.
var ErrFileNotFound = errors.New("file not found")

// FileFetcher reads one file from a repo at a given ref, returning
// ErrFileNotFound when it does not exist there.
//
// repo and token are meaningful to a fetcher that talks to a forge and ignored
// by one reading a local mirror, which authenticates when it fetches rather
// than per file. Both stay in the signature so one handler serves both.
type FileFetcher interface {
	FetchFileAtRef(ctx context.Context, repo, path, ref, token string) (string, error)
}
