package netaccept

import (
	"errors"
	"net"
	"syscall"
	"time"
)

// retryDelay is how long an accept loop pauses before retrying after a
// TEMPORARY failure. Long enough that a descriptor has a chance to be released,
// short enough that recovery is not noticeable once pressure drops.
const retryDelay = 50 * time.Millisecond

// Temporary reports whether an Accept error is worth retrying, and how
// long to wait first.
//
// It exists because the obvious `if err != nil { return }` conflates two
// opposite situations. Running out of file descriptors (EMFILE/ENFILE, which a
// burst of connections reaches on a host whose default limit is 1024) is
// TEMPORARY: a descriptor frees up as soon as any connection closes. Returning
// there ends the accept loop for good — the listener stays bound but nothing
// ever picks up again, so a front door serving every space goes dark until a
// human restarts it. A transient cause must not produce a permanent outage.
//
// A closed listener, by contrast, is final: Unregister or shutdown ran, and
// there is genuinely nothing left to accept.
func Temporary(err error) (time.Duration, bool) {
	if err == nil || errors.Is(err, net.ErrClosed) {
		return 0, false
	}
	if errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) ||
		errors.Is(err, syscall.ENOBUFS) || errors.Is(err, syscall.ENOMEM) ||
		errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EINTR) {
		return retryDelay, true
	}
	// net.Error still reports Timeout for a deadline-bearing listener.
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return retryDelay, true
	}
	return 0, false
}
