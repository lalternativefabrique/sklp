package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/internal/config"
)

// pinXDG points config.Path() at a temp dir for the duration of the test.
func pinXDG(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv(config.EnvContext, "")
	t.Setenv(config.EnvToken, "")
	t.Setenv(config.EnvContextToken, "")
	return dir
}

// meHandler returns the canonical /api/me payload on valid bearer,
// or 401 otherwise. The "valid" set is parameterised by the test.
func meHandler(t *testing.T, valid map[string]string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/me" {
			http.NotFound(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		token := strings.TrimPrefix(auth, "Bearer ")
		email, ok := valid[token]
		if !ok {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		w.WriteHeader(200)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"user_id": "u-1",
			"email":   email,
			"role":    "user",
		})
	}
}

func TestAuthLogin_Success(t *testing.T) {
	pinXDG(t)
	srv := httptest.NewServer(meHandler(t, map[string]string{
		"sklp_pat_abc_def": "alice@example.com",
	}))
	defer srv.Close()

	// Stdin: API URL (empty = default we supply via --api-url) + token line.
	stdin := bytes.NewBufferString("\nsklp_pat_abc_def\n")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	aio := &authIO{
		Stdin:        stdin,
		Stdout:       stdout,
		Stderr:       stderr,
		ReadPassword: func() (string, error) { return "sklp_pat_abc_def", nil },
	}
	if err := authLogin(aio, srv.URL); err != nil {
		t.Fatalf("authLogin: %v\nstderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "logged in as alice@example.com") {
		t.Fatalf("stdout missing success line: %s", stdout.String())
	}

	// Config written and contains the token.
	cfg, _, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.API.Token != "sklp_pat_abc_def" || cfg.API.URL != srv.URL {
		t.Fatalf("api block mismatch: %+v", cfg.API)
	}
	// Logging into the SaaS must not fabricate a deploy context.
	if len(cfg.Contexts) != 0 {
		t.Fatalf("auth login created deploy contexts: %+v", cfg.Contexts)
	}
}

func TestAuthLogin_RejectsBadPrefix(t *testing.T) {
	pinXDG(t)
	aio := &authIO{
		Stdin:        bytes.NewBufferString("\n"),
		Stdout:       &bytes.Buffer{},
		Stderr:       &bytes.Buffer{},
		ReadPassword: func() (string, error) { return "ghp_notapat", nil },
	}
	err := authLogin(aio, "http://localhost:0")
	if err == nil || !strings.Contains(err.Error(), "sklp_pat_") {
		t.Fatalf("expected sklp_pat_ prefix error, got: %v", err)
	}
}

func TestAuthLogin_TokenRejected(t *testing.T) {
	pinXDG(t)
	srv := httptest.NewServer(meHandler(t, map[string]string{})) // none valid
	defer srv.Close()

	aio := &authIO{
		Stdin:        bytes.NewBufferString("\n"),
		Stdout:       &bytes.Buffer{},
		Stderr:       &bytes.Buffer{},
		ReadPassword: func() (string, error) { return "sklp_pat_bad_secret", nil },
	}
	err := authLogin(aio, srv.URL)
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("expected rejection error, got: %v", err)
	}
}

func TestAuthStatus_OK(t *testing.T) {
	pinXDG(t)
	srv := httptest.NewServer(meHandler(t, map[string]string{
		"sklp_pat_abc_xyz": "bob@example.com",
	}))
	defer srv.Close()

	// Seed config.
	c := &config.Config{API: config.API{URL: srv.URL, Token: "sklp_pat_abc_xyz"}}
	path, _ := config.Path()
	if err := c.Save(path); err != nil {
		t.Fatalf("seed save: %v", err)
	}

	stdout := &bytes.Buffer{}
	aio := &authIO{Stdin: bytes.NewBufferString(""), Stdout: stdout, Stderr: &bytes.Buffer{}}
	if err := authStatus(aio); err != nil {
		t.Fatalf("authStatus: %v", err)
	}
	if !strings.Contains(stdout.String(), "bob@example.com") {
		t.Fatalf("stdout: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), srv.URL) {
		t.Fatalf("stdout missing api url: %s", stdout.String())
	}
}

func TestAuthStatus_NotLoggedIn(t *testing.T) {
	pinXDG(t)
	c := &config.Config{API: config.API{URL: "http://u"}}
	path, _ := config.Path()
	_ = c.Save(path)

	aio := &authIO{Stdin: bytes.NewBufferString(""), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	err := authStatus(aio)
	if err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("expected not-logged-in, got: %v", err)
	}
}

func TestAuthStatus_Rejected(t *testing.T) {
	pinXDG(t)
	srv := httptest.NewServer(meHandler(t, map[string]string{})) // none valid
	defer srv.Close()

	c := &config.Config{API: config.API{URL: srv.URL, Token: "sklp_pat_bad"}}
	path, _ := config.Path()
	_ = c.Save(path)

	err := authStatus(&authIO{Stdin: bytes.NewBufferString(""), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("expected rejected, got: %v", err)
	}
}

func TestAuthLogout_RemovesOnlyToken(t *testing.T) {
	pinXDG(t)
	c := &config.Config{
		API: config.API{URL: "http://x", Token: "sklp_pat_x"},
		Contexts: map[string]config.Context{
			"prod": {Server: "https://vm:9443", Token: "node-bearer"},
		},
	}
	path, _ := config.Path()
	if err := c.Save(path); err != nil {
		t.Fatalf("seed: %v", err)
	}

	stdout := &bytes.Buffer{}
	aio := &authIO{Stdin: bytes.NewBufferString(""), Stdout: stdout, Stderr: &bytes.Buffer{}}
	if err := authLogout(aio); err != nil {
		t.Fatalf("authLogout: %v", err)
	}
	got, _, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.API.Token != "" {
		t.Fatalf("api token not cleared: %+v", got.API)
	}
	if got.API.URL != "http://x" {
		t.Fatalf("api url should be kept: %+v", got.API)
	}
	// A SaaS logout must not disarm the deploy transport.
	if got.Contexts["prod"].Token != "node-bearer" {
		t.Fatalf("deploy context token was cleared by auth logout: %+v", got.Contexts["prod"])
	}
	if !strings.Contains(stdout.String(), "logged out") {
		t.Fatalf("stdout: %s", stdout.String())
	}

	// Ensure file still exists at the expected path.
	if _, err := os.Stat(filepath.Clean(path)); err != nil {
		t.Fatalf("config file missing: %v", err)
	}
}

// Reference the bytes package so go vet stays quiet across refactors.
var _ = bytes.NewBuffer

// Compile-time guard: a nil *Config should never satisfy any test
// (defensive; if someone refactors away the seed path the test should fail).
type Config = config.Config
