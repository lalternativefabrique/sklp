//go:build linux

package control

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeStoppedSpaceManifest marks a space as really deployed on this node: the
// manifest is what separates a space that merely stopped from a name nobody
// ever deployed.
func writeStoppedSpaceManifest(t *testing.T, space string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Dir(ManifestPath(space)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ManifestPath(space), []byte(`{"services":[{"name":"api"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

func downPath(space string) string {
	return SpaceRoutePrefix(space) + strings.TrimPrefix(RouteDown, "/v1")
}

// A stopped space is absent from the registry, so a delete aimed at one used to
// 404 — which left the only spaces anyone wants deleted as the ones no command
// could remove. It must materialise first, so the teardown has something to
// tear down.
func TestDownMaterialisesAStoppedSpace(t *testing.T) {
	writeStoppedSpaceManifest(t, "ghost")
	reg := NewRegistry()
	var asked string
	h := newMultiMux(reg, func(space string) error {
		asked = space
		reg.RegisterWithDelete(space, &fakeSupervisor{}, func() {}, func() {}, func() {})
		return nil
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, downPath("ghost"), strings.NewReader(`{"purge":true,"volumes":true}`)))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d (%s), want 202: a stopped space must still be deletable", rec.Code, rec.Body.String())
	}
	if asked != "ghost" {
		t.Fatalf("materialize called with %q, want the stopped space brought up for its teardown", asked)
	}
}

// The typo guard this reopens must stay shut: a name no deploy created has no
// manifest, and materialising it would CREATE an empty space instead of
// reporting the mistake (a stray "nginx"/"sapcea" space, node-d).
func TestDownOnANeverDeployedSpaceStill404s(t *testing.T) {
	writeStoppedSpaceManifest(t, "real")
	reg := NewRegistry()
	called := false
	h := newMultiMux(reg, func(string) error { called = true; return nil })

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, downPath("sapcea"), strings.NewReader(`{"purge":true,"volumes":true}`)))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a space no deploy ever created", rec.Code)
	}
	if called {
		t.Fatal("a mistyped space name must never be materialised by a delete")
	}
}

// SpaceIsPersisted is the whole guard: it answers for what is on disk, not for
// what happens to be running.
func TestSpaceIsPersisted(t *testing.T) {
	writeStoppedSpaceManifest(t, "deployed")
	if !SpaceIsPersisted("deployed") {
		t.Error("a space with a manifest must read as persisted even with no supervisor")
	}
	if SpaceIsPersisted("never-deployed") {
		t.Error("a name with no manifest must not read as persisted")
	}
	if SpaceIsPersisted("") {
		t.Error("an empty space name must not read as persisted")
	}
}
