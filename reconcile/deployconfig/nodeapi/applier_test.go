package nodeapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/reconcile/deployconfig/domain"
)

func TestControlApplierSendsTheRawFileAndItsVars(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = io.WriteString(w, `{"result":"configured","changes":["updated gateway routes"]}`)
	}))
	defer srv.Close()

	raw := "services:\n  web:\n    image: img:1\n"
	m := &domain.Manifest{Space: "app-production", Raw: []byte(raw), Vars: map[string]string{"APP_HOST": "example.org"}}
	a := &ControlApplier{client: srv.Client()}
	res, err := a.Apply(context.Background(), domain.NodeTarget{ServerURL: srv.URL, Space: "app-production", Token: "tok", Stack: "prod"}, m)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if gotPath != "/v1/spaces/app-production/apply" || gotAuth != "Bearer tok" {
		t.Fatalf("request = %s %q", gotPath, gotAuth)
	}
	src, _ := gotBody["source"].(map[string]any)
	if src["yaml"] != raw || src["stack"] != "prod" {
		t.Fatalf("source = %v", src)
	}
	if vars, _ := src["vars"].(map[string]any); vars["APP_HOST"] != "example.org" {
		t.Fatalf("vars = %v", src["vars"])
	}
	if _, hasSpec := gotBody["spec"]; hasSpec {
		t.Fatal("the mirrored spec must be gone: the node resolves the source itself")
	}
	if res.Result != "configured" || len(res.Changes) != 1 {
		t.Fatalf("result = %+v", res)
	}
}

func TestControlApplierSurfacesANodeRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "resolve deploy source: deploy file references unset environment variable(s): APP_HOST", http.StatusBadRequest)
	}))
	defer srv.Close()

	a := &ControlApplier{client: srv.Client()}
	_, err := a.Apply(context.Background(), domain.NodeTarget{ServerURL: srv.URL, Space: "s"}, &domain.Manifest{Raw: []byte("x: 1")})
	if err == nil || !strings.Contains(err.Error(), "APP_HOST") {
		t.Fatalf("err = %v, want the node's refusal surfaced", err)
	}
}
