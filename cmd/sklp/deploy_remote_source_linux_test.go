package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/lalternativefabrique/sklp/spacemanager/control"
)

const remoteSourceDeployYAML = `env_files: [.env]
services:
  web:
    image: rg.example.com/app/web:1
    port: 8080
    env:
      DB_PASS: ${DB_PASS}
      PUBLIC_URL: https://${APP_HOST}/${SPACE}
`

func TestRunRemoteDeploySendsTheRawFileAndItsVars(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("APP_HOST", "from-host.example.org")
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".sklp", "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".sklp", "web", "deploy.yaml"), []byte(remoteSourceDeployYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("DB_PASS=from-dotenv\nAPP_HOST=ignored\nUNRELATED=never-sent\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var got control.ApplyRequest
	var path, auth string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode: %v", err)
		}
		_ = json.NewEncoder(w).Encode(control.ApplyResult{Result: control.ResultCreated, Changes: []string{"added service web"}})
	}))
	defer srv.Close()
	sum := sha256.Sum256(srv.Certificate().Raw)

	code := runRemoteDeploy(deployTransport{
		stack:    "web",
		space:    "demo",
		repo:     repo,
		force:    true,
		server:   srv.URL,
		token:    "tok",
		caFinger: hex.EncodeToString(sum[:]),
	})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if path != "/v1/spaces/demo/apply" || auth != "Bearer tok" {
		t.Fatalf("hit %s with %q", path, auth)
	}
	if got.Source == nil || (len(got.Spec) != 0 && string(got.Spec) != "null") {
		t.Fatalf("want a source request and no spec, got %+v", got)
	}
	if got.Source.YAML != remoteSourceDeployYAML || got.Source.Stack != "web" || !got.Force {
		t.Fatalf("source not sent verbatim: %+v force=%v", got.Source, got.Force)
	}
	want := map[string]string{"DB_PASS": "from-dotenv", "APP_HOST": "from-host.example.org"}
	if len(got.Source.Vars) != len(want) {
		t.Fatalf("vars = %v, want exactly %v", got.Source.Vars, want)
	}
	for k, v := range want {
		if got.Source.Vars[k] != v {
			t.Fatalf("vars[%s] = %q, want %q", k, got.Source.Vars[k], v)
		}
	}
}

func TestRunRemoteDeployRefusesUnsetVarsBeforeSending(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	repo := t.TempDir()
	_ = os.MkdirAll(filepath.Join(repo, ".sklp", "web"), 0o755)
	_ = os.WriteFile(filepath.Join(repo, ".sklp", "web", "deploy.yaml"), []byte("services:\n  web:\n    image: x\n    env:\n      A: ${SKLP_TEST_NEVER_SET}\n"), 0o644)
	called := false
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()
	sum := sha256.Sum256(srv.Certificate().Raw)

	code := runRemoteDeploy(deployTransport{stack: "web", space: "demo", repo: repo, server: srv.URL, token: "tok", caFinger: hex.EncodeToString(sum[:])})
	if code == 0 || called {
		t.Fatalf("exit %d, node called=%v: an unset var must stop the deploy on the client", code, called)
	}
}
