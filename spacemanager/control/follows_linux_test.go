package control

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeFollows struct {
	added   []FollowRequest
	removed []string
}

func (f *fakeFollows) ListFollows(context.Context) (any, error) {
	return []string{"hello"}, nil
}

func (f *fakeFollows) AddFollow(_ context.Context, req FollowRequest) (any, error) {
	switch req.Repo {
	case "bad":
		return nil, fmt.Errorf("%w: not a clone URL", ErrFollowInvalid)
	case "taken":
		return nil, fmt.Errorf("%w: name in use", ErrFollowConflict)
	}
	f.added = append(f.added, req)
	return req, nil
}

func (f *fakeFollows) RemoveFollow(_ context.Context, name string) error {
	if name == "ghost" {
		return fmt.Errorf("%w: %s", ErrFollowMissing, name)
	}
	f.removed = append(f.removed, name)
	return nil
}

func TestFollowsRouteMapsEachOutcome(t *testing.T) {
	SetFollows(nil)
	rec := httptest.NewRecorder()
	serveFollows(rec, httptest.NewRequest(http.MethodGet, RouteFollows, nil))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("no source: %d, want 501", rec.Code)
	}

	src := &fakeFollows{}
	SetFollows(src)
	t.Cleanup(func() { SetFollows(nil) })
	cases := []struct {
		method, path, body string
		want               int
	}{
		{http.MethodGet, RouteFollows, "", http.StatusOK},
		{http.MethodPost, RouteFollows, `{"repo":"https://github.com/moi/hello"}`, http.StatusCreated},
		{http.MethodPost, RouteFollows, `{"repo":"bad"}`, http.StatusBadRequest},
		{http.MethodPost, RouteFollows, `{"repo":"taken"}`, http.StatusConflict},
		{http.MethodPost, RouteFollows, `not json`, http.StatusBadRequest},
		{http.MethodDelete, RouteFollows + "/hello", "", http.StatusNoContent},
		{http.MethodDelete, RouteFollows + "/ghost", "", http.StatusNotFound},
		{http.MethodDelete, RouteFollows, "", http.StatusMethodNotAllowed},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		serveFollows(rec, httptest.NewRequest(c.method, c.path, strings.NewReader(c.body)))
		if rec.Code != c.want {
			t.Errorf("%s %s %s: %d, want %d (%s)", c.method, c.path, c.body, rec.Code, c.want, rec.Body.String())
		}
	}
	if len(src.added) != 1 || src.added[0].Repo != "https://github.com/moi/hello" || len(src.removed) != 1 || src.removed[0] != "hello" {
		t.Fatalf("source saw added=%+v removed=%v", src.added, src.removed)
	}
}
