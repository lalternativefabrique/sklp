package control

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestValidSpaceName(t *testing.T) {
	for _, ok := range []string{"production", "acme-production", "www", "nats_global", "Staging1"} {
		if err := validSpaceName(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", ".", "..", "../x", "a/b", "a.b", "-x", "a b"} {
		if err := validSpaceName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestMultiSpace_TraversingSpaceNameRejected(t *testing.T) {
	handler := newMultiMux(NewRegistry(), nil)
	for _, path := range []string{"/v1/spaces/../manifest", "/v1/spaces/../down", "/v1/spaces/./apply"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://node"+path, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", path, rec.Code)
		}
	}
}

func TestRemoveSpaceArtifactsRefusesTraversal(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := RemoveSpaceArtifacts(".."); err == nil {
		t.Fatal(`RemoveSpaceArtifacts("..") succeeded`)
	}
}
