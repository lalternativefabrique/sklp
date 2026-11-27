package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeWatches []WatchEntry

func (f fakeWatches) ListWatches(context.Context) ([]WatchEntry, error) { return f, nil }

func TestWatchesRouteListsAnEmptySetWithoutASource(t *testing.T) {
	SetWatches(nil)
	rec := httptest.NewRecorder()
	serveWatches(rec, httptest.NewRequest(http.MethodGet, RouteWatches, nil))
	var body WatchesResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil || body.Watches == nil {
		t.Fatalf("no source: %d %s", rec.Code, rec.Body)
	}

	SetWatches(fakeWatches{{Name: "web", Space: "hello"}})
	t.Cleanup(func() { SetWatches(nil) })
	rec = httptest.NewRecorder()
	serveWatches(rec, httptest.NewRequest(http.MethodGet, RouteWatches, nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Watches) != 1 || body.Watches[0].Space != "hello" {
		t.Fatalf("with source: %s", rec.Body)
	}
	rec = httptest.NewRecorder()
	serveWatches(rec, httptest.NewRequest(http.MethodPost, RouteWatches, nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", rec.Code)
	}
}
