package secrets

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/reconcile/deployconfig/domain"
)

// skalpaiAPI stands in for core's endpoint, recording what the provider sent.
type skalpaiAPI struct {
	status  int
	secrets map[string]string

	gotAuth      string
	gotBody      resolveRequest
	gotPath      string
	requestCount int
}

func (a *skalpaiAPI) start(t *testing.T) *SkalpaiHTTPProvider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.requestCount++
		a.gotAuth = r.Header.Get("Authorization")
		a.gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&a.gotBody)

		status := a.status
		if status == 0 {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			_ = json.NewEncoder(w).Encode(resolveResponse{Secrets: a.secrets})
		}
	}))
	t.Cleanup(srv.Close)
	return &SkalpaiHTTPProvider{BaseURL: srv.URL, Token: "sklp_node_test", ProjectID: "p1", Client: srv.Client()}
}

func request() domain.SecretRequest {
	return domain.SecretRequest{Reason: "reconcile acme/widgets@abc"}
}

func TestFetchReturnsTheProjectSecrets(t *testing.T) {
	api := &skalpaiAPI{secrets: map[string]string{"JWT_SECRET": "s3cret"}}
	p := api.start(t)

	got, err := p.Fetch(context.Background(), domain.SecretStore{}, request(), nil)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got["JWT_SECRET"] != "s3cret" {
		t.Fatalf("got %v, want the resolved secret", got)
	}
	if api.gotPath != "/api/internal/nodes/secrets/resolve" {
		t.Errorf("path = %q, want the node endpoint", api.gotPath)
	}
}

// The token goes in a header, not the URL: a URL is what ends up in access
// logs, proxy traces and error messages.
func TestFetchSendsTheTokenAsABearer(t *testing.T) {
	api := &skalpaiAPI{secrets: map[string]string{}}
	p := api.start(t)

	if _, err := p.Fetch(context.Background(), domain.SecretStore{}, request(), nil); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if api.gotAuth != "Bearer sklp_node_test" {
		t.Fatalf("Authorization = %q, want the node token as a bearer", api.gotAuth)
	}
}

// The reason reaches the audit log, so a read afterwards is explicable and not
// merely attributable.
func TestFetchSendsTheReason(t *testing.T) {
	api := &skalpaiAPI{secrets: map[string]string{}}
	p := api.start(t)

	if _, err := p.Fetch(context.Background(), domain.SecretStore{}, request(), nil); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if api.gotBody.Reason != "reconcile acme/widgets@abc" {
		t.Errorf("reason = %q, want what the reconcile was doing", api.gotBody.Reason)
	}
	if api.gotBody.ProjectID != "p1" {
		t.Errorf("project = %q, want the node's configured project", api.gotBody.ProjectID)
	}
}

// Each status says something different to whoever has to fix it, so each gets
// its own message rather than a generic "request failed".
func TestFetchExplainsEachRefusal(t *testing.T) {
	for status, want := range map[int]string{
		http.StatusUnauthorized: "SKLP_NODE_TOKEN",
		http.StatusForbidden:    "does not allow reading secrets",
		http.StatusNotFound:     "not in this node's stack",
	} {
		api := &skalpaiAPI{status: status}
		p := api.start(t)

		_, err := p.Fetch(context.Background(), domain.SecretStore{}, request(), nil)
		if err == nil {
			t.Fatalf("status %d: want an error", status)
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("status %d: err = %v, want it to mention %q", status, err, want)
		}
	}
}

// A node with no token must say so rather than sending an unauthenticated
// request and reporting whatever came back.
func TestFetchRefusesWithoutAToken(t *testing.T) {
	api := &skalpaiAPI{secrets: map[string]string{}}
	p := api.start(t)
	p.Token = ""

	_, err := p.Fetch(context.Background(), domain.SecretStore{}, request(), nil)
	if err == nil || !strings.Contains(err.Error(), "SKLP_NODE_TOKEN") {
		t.Fatalf("err = %v, want it to name the variable to set", err)
	}
	if api.requestCount != 0 {
		t.Error("an unauthenticated request was sent anyway")
	}
}

// A project with nothing deploy-readable resolves to an empty map, not nil: a
// manifest referencing no ${VAR} must reconcile, and a nil would read as a
// failure at the call site.
func TestFetchReturnsAnEmptyMapWhenNothingIsReadable(t *testing.T) {
	api := &skalpaiAPI{secrets: nil}
	p := api.start(t)

	got, err := p.Fetch(context.Background(), domain.SecretStore{}, request(), nil)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got == nil {
		t.Fatal("got nil, want an empty map")
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want nothing", got)
	}
}

// An unreachable API is a failed pass, not a silent empty resolve — which
// would deploy a manifest with its ${VAR} unresolved.
func TestFetchFailsWhenSkalpaiIsUnreachable(t *testing.T) {
	p := &SkalpaiHTTPProvider{BaseURL: "http://127.0.0.1:1", Token: "sklp_node_test", ProjectID: "p1"}

	if _, err := p.Fetch(context.Background(), domain.SecretStore{}, request(), nil); err == nil {
		t.Fatal("want an error when the API cannot be reached")
	}
}
