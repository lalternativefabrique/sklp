package netaccept

import (
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"
)

// Running out of descriptors must be retried: it clears as soon as any
// connection closes, and giving up takes the front door down for good.
func TestTemporary_RetriesDescriptorExhaustion(t *testing.T) {
	for _, err := range []error{syscall.EMFILE, syscall.ENFILE, syscall.ENOBUFS, syscall.ENOMEM} {
		delay, retry := Temporary(err)
		if !retry {
			t.Errorf("Temporary(%v) = false; descriptor/memory exhaustion is transient and must be retried", err)
		}
		if delay <= 0 {
			t.Errorf("Temporary(%v) gave delay %v; a retry must back off, not spin", err, delay)
		}
	}
}

// Wrapped by *net.OpError, which is how the runtime actually returns it.
func TestTemporary_UnwrapsOpError(t *testing.T) {
	wrapped := &net.OpError{Op: "accept", Err: syscall.EMFILE}
	if _, retry := Temporary(wrapped); !retry {
		t.Error("a wrapped EMFILE must still be recognised — the runtime never returns it bare")
	}
	if _, retry := Temporary(fmt.Errorf("accept: %w", syscall.EMFILE)); !retry {
		t.Error("errors.Is wrapping must be honoured")
	}
}

// A closed listener is final: shutdown or Unregister ran, and looping would
// spin forever on an error that never clears.
func TestTemporary_StopsOnClosedListener(t *testing.T) {
	if _, retry := Temporary(net.ErrClosed); retry {
		t.Error("a closed listener must end the loop, not retry")
	}
	if _, retry := Temporary(&net.OpError{Op: "accept", Err: net.ErrClosed}); retry {
		t.Error("a wrapped ErrClosed must end the loop too")
	}
	if _, retry := Temporary(nil); retry {
		t.Error("nil is not a retryable error")
	}
	if _, retry := Temporary(errors.New("some permanent failure")); retry {
		t.Error("an unknown error must not be retried forever")
	}
}
