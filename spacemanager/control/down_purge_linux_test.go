//go:build linux

package control

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// flag is a teardown marker written by the handler's goroutine and read by the
// test's. The Down route acks BEFORE running the teardown, so the two really do
// race — a plain bool here is a data race the race detector fails on.
type flag struct{ v atomic.Bool }

func (f *flag) set()      { f.v.Store(true) }
func (f *flag) get() bool { return f.v.Load() }

// waitFor polls cond: the Down handler acks BEFORE triggering the teardown,
// which then runs in its own goroutine.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the teardown to fire")
}

// The UI's Stop button and `sklp deploy delete` share this route. Stopping
// must keep the space's manifest so it can be started again — erasing it left
// the space gone from every list with no way back to it.
func TestDownWithoutPurgeStops(t *testing.T) {
	var stopped, purged flag
	mux := newMuxWithPurge("production", &fakeSupervisor{},
		stopped.set,
		purged.set,
	)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, RouteDown, strings.NewReader(`{"purge":false}`)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	waitFor(t, func() bool { return stopped.get() })
	if purged.get() {
		t.Fatal("a stop must not purge the space's persisted state")
	}
}

func TestDownWithPurgeDeletes(t *testing.T) {
	var stopped, purged flag
	mux := newMuxWithPurge("production", &fakeSupervisor{},
		stopped.set,
		purged.set,
	)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, RouteDown, strings.NewReader(`{"purge":true}`)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	waitFor(t, func() bool { return purged.get() })
	if stopped.get() {
		t.Fatal("purge must take the deleting teardown, not the plain stop")
	}
}

// An empty body is the historical shape of this route. It must stop, never
// delete: a caller that did not ask for a deletion does not get one.
func TestDownWithNoBodyStops(t *testing.T) {
	var stopped, purged flag
	mux := newMuxWithPurge("production", &fakeSupervisor{},
		stopped.set,
		purged.set,
	)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, RouteDown, nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	waitFor(t, func() bool { return stopped.get() })
	if purged.get() {
		t.Fatal("an unqualified down must not delete the space")
	}
}

// A node with no deleting teardown wired (single-space path) still answers a
// purge request by stopping, rather than failing.
func TestDownPurgeFallsBackWhenUnwired(t *testing.T) {
	var stopped flag
	mux := newMuxWithPurge("production", &fakeSupervisor{}, stopped.set, nil)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, RouteDown, strings.NewReader(`{"purge":true}`)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	waitFor(t, func() bool { return stopped.get() })
}
