//go:build linux

package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPublicNICRoute(t *testing.T) {
	h := newMultiMux(NewRegistry(), nil)
	t.Cleanup(func() { SetPublicNIC(nil) })

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, RoutePublicNIC, nil))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("unset: %d", rec.Code)
	}

	SetPublicNIC(func() (json.RawMessage, error) { return json.RawMessage(`{"repairs":2}`), nil })
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, RoutePublicNIC, nil))
	var body PublicNICResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("%d %s %v", rec.Code, rec.Body, err)
	}
	if !strings.Contains(string(body.Status), `"repairs":2`) {
		t.Fatalf("status %s", body.Status)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, RoutePublicNIC, nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", rec.Code)
	}
}
