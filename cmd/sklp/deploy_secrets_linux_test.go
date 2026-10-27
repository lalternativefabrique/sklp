package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/lalternativefabrique/sklp/internal/config"
)

func resolveHandler(t *testing.T, secrets map[string]string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/projects/proj-1/secrets/resolve" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(200)
		_ = json.NewEncoder(w).Encode(map[string]any{"secrets": secrets})
	}
}

func writeDeployYAML(t *testing.T, projectID string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "deploy.yaml")
	body := "services: {}\n"
	if projectID != "" {
		body = "project_id: " + projectID + "\nservices: {}\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadProjectSecretsIntoEnv_NoProjectIDIsNoOp(t *testing.T) {
	pinXDG(t)
	path := writeDeployYAML(t, "")
	loadProjectSecretsIntoEnv(path)
	if _, set := os.LookupEnv("SCW_SECRET_KEY"); set {
		t.Fatal("no project_id: must not touch the env at all")
	}
}

func TestLoadProjectSecretsIntoEnv_NotLoggedInIsNoOp(t *testing.T) {
	pinXDG(t)
	path := writeDeployYAML(t, "proj-1")
	loadProjectSecretsIntoEnv(path)
	if _, set := os.LookupEnv("SCW_SECRET_KEY"); set {
		t.Fatal("no token configured must not set anything, not fail the deploy")
	}
}

func TestLoadProjectSecretsIntoEnv_FetchesAndSetsEnv(t *testing.T) {
	pinXDG(t)
	srv := httptest.NewServer(resolveHandler(t, map[string]string{"SCW_SECRET_KEY": "from-core"}))
	defer srv.Close()

	cfg := &config.Config{API: config.API{URL: srv.URL, Token: "sklp_pat_x"}}
	cfgPath, _ := config.Path()
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatalf("save config: %v", err)
	}

	os.Unsetenv("SCW_SECRET_KEY")

	deployPath := writeDeployYAML(t, "proj-1")
	loadProjectSecretsIntoEnv(deployPath)

	if got := os.Getenv("SCW_SECRET_KEY"); got != "from-core" {
		t.Fatalf("SCW_SECRET_KEY = %q, want %q", got, "from-core")
	}
}

func TestLoadProjectSecretsIntoEnv_HostEnvWins(t *testing.T) {
	pinXDG(t)
	srv := httptest.NewServer(resolveHandler(t, map[string]string{"SCW_SECRET_KEY": "from-core"}))
	defer srv.Close()

	cfg := &config.Config{API: config.API{URL: srv.URL, Token: "sklp_pat_x"}}
	cfgPath, _ := config.Path()
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatalf("save config: %v", err)
	}

	t.Setenv("SCW_SECRET_KEY", "already-set-by-operator")

	deployPath := writeDeployYAML(t, "proj-1")
	loadProjectSecretsIntoEnv(deployPath)

	if got := os.Getenv("SCW_SECRET_KEY"); got != "already-set-by-operator" {
		t.Fatalf("host env was overwritten: got %q", got)
	}
}
