package tags

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestParseChallenge covers the two realms this client actually meets: Docker
// Hub's auth.docker.io and Scaleway's api.scaleway.com/registry-internal, whose
// path is what a client assuming the /v2/token convention gets wrong.
func TestParseChallenge(t *testing.T) {
	cases := []struct {
		header         string
		realm, service string
	}{
		{
			`Bearer realm="https://api.scaleway.com/registry-internal/v1/regions/fr-par/tokens",service="registry"`,
			"https://api.scaleway.com/registry-internal/v1/regions/fr-par/tokens", "registry",
		},
		{
			`Bearer realm="https://auth.docker.io/token",service="registry.docker.io"`,
			"https://auth.docker.io/token", "registry.docker.io",
		},
		{`Basic realm="x"`, "", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		realm, service := parseChallenge(c.header)
		if realm != c.realm || service != c.service {
			t.Errorf("parseChallenge(%q) = %q/%q, want %q/%q", c.header, realm, service, c.realm, c.service)
		}
	}
}

func TestSplitRepo(t *testing.T) {
	valid := map[string][2]string{
		"rg.fr-par.scw.cloud/lalternative/www": {"rg.fr-par.scw.cloud", "lalternative/www"},
		"registry.internal:5000/ns/app":        {"registry.internal:5000", "ns/app"},
		"localhost/ns/app":                     {"localhost", "ns/app"},
	}
	for repo, want := range valid {
		host, path, err := splitRepo(repo)
		if err != nil {
			t.Errorf("splitRepo(%q) = %v", repo, err)
			continue
		}
		if host != want[0] || path != want[1] {
			t.Errorf("splitRepo(%q) = %q/%q, want %q/%q", repo, host, path, want[0], want[1])
		}
	}
	// A bare name must not silently default to Docker Hub: a watch always says
	// where it pulls from.
	for _, repo := range []string{"www", "nginx", ""} {
		if _, _, err := splitRepo(repo); err == nil {
			t.Errorf("splitRepo(%q) = nil, want an error", repo)
		}
	}
}

// TestListTagsAuthFlow drives the full 401 → token → retry sequence against a
// fake registry, asserting the client scopes the token to a pull on that one
// repository and replays the request with the bearer it was handed.
func TestListTagsAuthFlow(t *testing.T) {
	var gotScope, gotService, gotAuthHeader string
	var tagsCalls int

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/auth", func(w http.ResponseWriter, r *http.Request) {
		gotScope = r.URL.Query().Get("scope")
		gotService = r.URL.Query().Get("service")
		if u, p, ok := r.BasicAuth(); !ok || u != "nologin" || p != "s3cr3t" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "minted-token"})
	})
	mux.HandleFunc("/v2/ns/app/tags/list", func(w http.ResponseWriter, r *http.Request) {
		tagsCalls++
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/auth",service="registry"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		gotAuthHeader = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"tags": []string{"20260818-0331-d205d58", "latest"}})
	})

	c := NewClient()
	// httptest serves plain HTTP; point the client at it by overriding the
	// scheme the way the production path builds it.
	c.baseScheme = "http"

	tags, err := c.ListTags(context.Background(), hostOf(srv.URL)+"/ns/app", "nologin", "s3cr3t")
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	if len(tags) != 2 || tags[0] != "20260818-0331-d205d58" {
		t.Fatalf("tags = %v", tags)
	}
	if gotScope != "repository:ns/app:pull" {
		t.Errorf("scope = %q, want a pull scoped to this repository alone", gotScope)
	}
	if gotService != "registry" {
		t.Errorf("service = %q, want the one the challenge advertised", gotService)
	}
	if gotAuthHeader != "Bearer minted-token" {
		t.Errorf("retry Authorization = %q", gotAuthHeader)
	}
	if tagsCalls != 2 {
		t.Errorf("tags endpoint called %d times, want 2 (challenge then retry)", tagsCalls)
	}
}

// TestListTagsAnonymous: a public repository answers without a challenge, and
// the client must not attempt a token exchange.
func TestListTagsAnonymous(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"tags": []string{"1.0.0"}})
	}))
	defer srv.Close()

	c := NewClient()
	c.baseScheme = "http"
	tags, err := c.ListTags(context.Background(), hostOf(srv.URL)+"/ns/app", "", "")
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	if len(tags) != 1 || tags[0] != "1.0.0" {
		t.Fatalf("tags = %v", tags)
	}
}

func hostOf(rawURL string) string {
	return rawURL[len("http://"):]
}

// TestAuthFailureDoesNotLeakBody: this error is stored on the watch row and
// rendered in the UI, so an auth endpoint echoing the rejected credential must
// not travel with it. The status is what diagnoses a 401.
func TestAuthFailureDoesNotLeakBody(t *testing.T) {
	const leaked = "invalid secret scw-SECRET-VALUE for user nologin"

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/auth", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(leaked))
	})
	mux.HandleFunc("/v2/ns/app/tags/list", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/auth",service="registry"`)
		w.WriteHeader(http.StatusUnauthorized)
	})

	c := NewClient()
	c.baseScheme = "http"

	_, err := c.ListTags(context.Background(), hostOf(srv.URL)+"/ns/app", "nologin", "scw-SECRET-VALUE")
	if err == nil {
		t.Fatal("expected an error on a rejected credential")
	}
	if strings.Contains(err.Error(), leaked) || strings.Contains(err.Error(), "scw-SECRET-VALUE") {
		t.Fatalf("the auth response body reached the error: %v", err)
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("error %v does not carry the status that diagnoses it", err)
	}
}

// TestTagListFailureDoesNotLeakBody: same rule on the tag listing itself.
func TestTagListFailureDoesNotLeakBody(t *testing.T) {
	const leaked = "token scw-SECRET-VALUE is not authorised"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(leaked))
	}))
	defer srv.Close()

	c := NewClient()
	c.baseScheme = "http"

	_, err := c.ListTags(context.Background(), hostOf(srv.URL)+"/ns/app", "", "")
	if err == nil {
		t.Fatal("expected an error on a 403")
	}
	if strings.Contains(err.Error(), "scw-SECRET-VALUE") {
		t.Fatalf("the response body reached the error: %v", err)
	}
}
