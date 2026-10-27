package control

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeMeshDir struct{ peers map[string]MeshPeer }

func (f *fakeMeshDir) ListPeers(context.Context) ([]MeshPeer, error) {
	out := []MeshPeer{}
	for _, p := range f.peers {
		out = append(out, p)
	}
	return out, nil
}

func (f *fakeMeshDir) DeletePeer(_ context.Context, id string) error {
	if _, ok := f.peers[id]; !ok {
		return ErrMeshPeerNotFound
	}
	delete(f.peers, id)
	return nil
}

func TestServeMeshPeers(t *testing.T) {
	t.Cleanup(func() { InstallMeshDirectory(nil) })
	do := func(method, path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		serveMeshPeers(w, httptest.NewRequest(method, path, nil))
		return w
	}

	if w := do(http.MethodGet, RouteMeshPeers); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("no mesh: %d", w.Code)
	}
	InstallMeshDirectory(&fakeMeshDir{peers: map[string]MeshPeer{"dead": {ID: "dead", PrivateIP: "10.90.0.6"}}})

	if w := do(http.MethodGet, RouteMeshPeers); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "10.90.0.6") {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	if w := do(http.MethodDelete, RouteMeshPeers+"/dead"); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	if w := do(http.MethodDelete, RouteMeshPeers+"/dead"); w.Code != http.StatusNotFound {
		t.Fatalf("delete again: %d", w.Code)
	}
	if w := do(http.MethodDelete, RouteMeshPeers); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("delete without id: %d", w.Code)
	}
}
