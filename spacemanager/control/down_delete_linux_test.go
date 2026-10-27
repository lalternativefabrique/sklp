//go:build linux

package control

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Deleting a space must take the teardown that drops the named volumes too.
// Keeping them back held the space dir alive, and the inventory lists spaces
// by directory name — so the deleted space came back as a stopped ghost no
// command could remove.
func TestDownWithVolumesDeletesTheData(t *testing.T) {
	var stopped, purged, deleted flag
	mux := newMuxWithDelete("production", &fakeSupervisor{}, stopped.set, purged.set, deleted.set)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, RouteDown, strings.NewReader(`{"purge":true,"volumes":true}`)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	waitFor(t, func() bool { return deleted.get() })
	if stopped.get() || purged.get() {
		t.Fatal("a delete must take the volume-dropping teardown, not the stop or the data-keeping purge")
	}
}

// A purge without volumes keeps them: `sklp deploy delete` without -v has
// never dropped a space's data and must keep not doing so.
func TestDownPurgeWithoutVolumesKeepsTheData(t *testing.T) {
	var stopped, purged, deleted flag
	mux := newMuxWithDelete("production", &fakeSupervisor{}, stopped.set, purged.set, deleted.set)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, RouteDown, strings.NewReader(`{"purge":true}`)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	waitFor(t, func() bool { return purged.get() })
	if deleted.get() {
		t.Fatal("a purge that did not ask for the volumes must not destroy them")
	}
}

// volumes:true on a STOP is not a deletion: the Stop button promises the data
// survives, and a stray field must never turn that into data loss.
func TestDownVolumesWithoutPurgeStillOnlyStops(t *testing.T) {
	var stopped, purged, deleted flag
	mux := newMuxWithDelete("production", &fakeSupervisor{}, stopped.set, purged.set, deleted.set)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, RouteDown, strings.NewReader(`{"purge":false,"volumes":true}`)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	waitFor(t, func() bool { return stopped.get() })
	if purged.get() || deleted.get() {
		t.Fatal("volumes:true without purge must stop the space, never delete it")
	}
}

// A node with no volume-dropping teardown wired (the single-space path) still
// answers a delete by purging, rather than failing.
func TestDownDeleteFallsBackToPurgeWhenUnwired(t *testing.T) {
	var stopped, purged flag
	mux := newMuxWithDelete("production", &fakeSupervisor{}, stopped.set, purged.set, nil)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, RouteDown, strings.NewReader(`{"purge":true,"volumes":true}`)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	waitFor(t, func() bool { return purged.get() })
	if stopped.get() {
		t.Fatal("an unwired delete must fall back to the purge, not the plain stop")
	}
}
