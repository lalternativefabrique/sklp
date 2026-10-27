package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/internal/cli"
)

func secretArgs(t *testing.T, sub string, argv ...string) cli.Args {
	t.Helper()
	cmd := secretCommand()
	for _, c := range cmd.Sub {
		if c.Name == sub {
			a, err := cli.ParseLeaf(c, argv)
			if err != nil {
				t.Fatalf("parse secret %s %v: %v", sub, argv, err)
			}
			return a
		}
	}
	t.Fatalf("unknown secret sub-verb %q", sub)
	return nil
}

func writeSecretFile(t *testing.T, name string, kv map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	var b strings.Builder
	for k, v := range kv {
		b.WriteString(k + "=" + v + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSecretPush_CreatesNewSecrets(t *testing.T) {
	var created []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/projects/proj1/secrets":
			_ = json.NewEncoder(w).Encode([]any{})
		case r.Method == http.MethodPost && r.URL.Path == "/api/projects/proj1/secrets":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			created = append(created, body)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "sec-" + body["name"].(string), "name": body["name"]})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	writeAPIConfig(t, srv.URL)

	file := writeSecretFile(t, ".env.secret", map[string]string{"MY_TOKEN": "s3cr3t"})

	out := &bytes.Buffer{}
	iio := &issueIO{Stdout: out, Stderr: io.Discard}
	if err := secretPush(iio, secretArgs(t, "push", "--project", "proj1", file)); err != nil {
		t.Fatalf("push: %v", err)
	}
	if len(created) != 1 {
		t.Fatalf("expected 1 create call, got %d", len(created))
	}
	if created[0]["name"] != "MY_TOKEN" {
		t.Fatalf("unexpected name: %v", created[0]["name"])
	}
	if created[0]["deploy_readable"] != true {
		t.Fatalf("expected deploy_readable=true by default, got %v", created[0]["deploy_readable"])
	}
}

func TestSecretPush_UpdatesExistingSecrets(t *testing.T) {
	var patched bool
	var posted bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/projects/proj1/secrets":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "sec-1", "name": "MY_TOKEN", "value_last4": "abcd"}})
		case r.Method == http.MethodPatch && r.URL.Path == "/api/projects/proj1/secrets/sec-1":
			patched = true
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "sec-1", "name": "MY_TOKEN"})
		case r.Method == http.MethodPost:
			posted = true
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	writeAPIConfig(t, srv.URL)

	file := writeSecretFile(t, ".env.secret", map[string]string{"MY_TOKEN": "new-value"})

	out := &bytes.Buffer{}
	iio := &issueIO{Stdout: out, Stderr: io.Discard}
	if err := secretPush(iio, secretArgs(t, "push", "--project", "proj1", file)); err != nil {
		t.Fatalf("push: %v", err)
	}
	if !patched {
		t.Fatal("expected PATCH to be called")
	}
	if posted {
		t.Fatal("expected no POST call for an existing secret")
	}
}

func TestSecretPush_DeployReadableFlagOverridesDefault(t *testing.T) {
	var created map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode([]any{})
		case r.Method == http.MethodPost:
			_ = json.NewDecoder(r.Body).Decode(&created)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "sec-1", "name": "KUBECONFIG_X"})
		}
	}))
	defer srv.Close()
	writeAPIConfig(t, srv.URL)

	file := writeSecretFile(t, ".env.secret", map[string]string{"KUBECONFIG_X": "v"})

	out := &bytes.Buffer{}
	iio := &issueIO{Stdout: out, Stderr: io.Discard}
	if err := secretPush(iio, secretArgs(t, "push", "--project", "proj1", "--deploy-readable=false", file)); err != nil {
		t.Fatalf("push: %v", err)
	}
	if created["deploy_readable"] != false {
		t.Fatalf("expected deploy_readable=false, got %v", created["deploy_readable"])
	}
}

func TestSecretPush_DryRunMakesNoWriteCalls(t *testing.T) {
	var writeCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode([]any{})
		default:
			writeCalled = true
		}
	}))
	defer srv.Close()
	writeAPIConfig(t, srv.URL)

	file := writeSecretFile(t, ".env.secret", map[string]string{"MY_TOKEN": "v"})

	out := &bytes.Buffer{}
	iio := &issueIO{Stdout: out, Stderr: io.Discard}
	if err := secretPush(iio, secretArgs(t, "push", "--project", "proj1", "--dry-run", file)); err != nil {
		t.Fatalf("push: %v", err)
	}
	if writeCalled {
		t.Fatal("--dry-run must not call POST/PATCH")
	}
	if !strings.Contains(out.String(), "create MY_TOKEN") {
		t.Fatalf("expected dry-run output, got:\n%s", out.String())
	}
}

func TestSecretPush_NeverLogsPlaintext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode([]any{})
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "sec-1", "name": "MY_TOKEN"})
		}
	}))
	defer srv.Close()
	writeAPIConfig(t, srv.URL)

	file := writeSecretFile(t, ".env.secret", map[string]string{"MY_TOKEN": "leaked-value-must-not-appear"})

	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	iio := &issueIO{Stdout: out, Stderr: errOut}
	if err := secretPush(iio, secretArgs(t, "push", "--project", "proj1", file)); err != nil {
		t.Fatalf("push: %v", err)
	}
	if strings.Contains(out.String(), "leaked-value-must-not-appear") || strings.Contains(errOut.String(), "leaked-value-must-not-appear") {
		t.Fatal("push must never print plaintext")
	}
}

func TestSecretPull_WritesNewFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/projects/proj1/secrets/resolve" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"secrets": map[string]string{"MY_TOKEN": "resolved-value"}})
	}))
	defer srv.Close()
	writeAPIConfig(t, srv.URL)

	dir := t.TempDir()
	file := filepath.Join(dir, ".env.secret")

	out := &bytes.Buffer{}
	iio := &issueIO{Stdout: out, Stderr: io.Discard}
	if err := secretPull(iio, secretArgs(t, "pull", "--project", "proj1", file)); err != nil {
		t.Fatalf("pull: %v", err)
	}
	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "MY_TOKEN=resolved-value") {
		t.Fatalf("unexpected file content:\n%s", got)
	}
}

func TestSecretPull_OverwritesKnownKeyPreservingOthers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"secrets": map[string]string{"MY_TOKEN": "new-value"}})
	}))
	defer srv.Close()
	writeAPIConfig(t, srv.URL)

	dir := t.TempDir()
	file := filepath.Join(dir, ".env.secret")
	if err := os.WriteFile(file, []byte("# a comment\nMY_TOKEN=old-value\nLOCAL_ONLY=keep-me\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := &bytes.Buffer{}
	iio := &issueIO{Stdout: out, Stderr: io.Discard}
	if err := secretPull(iio, secretArgs(t, "pull", "--project", "proj1", file)); err != nil {
		t.Fatalf("pull: %v", err)
	}
	got, _ := os.ReadFile(file)
	s := string(got)
	for _, want := range []string{"# a comment", "MY_TOKEN=new-value", "LOCAL_ONLY=keep-me"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in:\n%s", want, s)
		}
	}
	if strings.Contains(s, "old-value") {
		t.Fatalf("stale value not overwritten:\n%s", s)
	}
}

func TestSecretPull_RequiresForceForNonDefaultFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"secrets": map[string]string{}})
	}))
	defer srv.Close()
	writeAPIConfig(t, srv.URL)

	dir := t.TempDir()
	file := filepath.Join(dir, ".env.prod")

	out := &bytes.Buffer{}
	iio := &issueIO{Stdout: out, Stderr: io.Discard}
	err := secretPull(iio, secretArgs(t, "pull", "--project", "proj1", file))
	if err == nil {
		t.Fatal("expected an error without --force for a non-default filename")
	}
}

func TestSecretList_RendersLast4Only(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": "sec-1", "name": "MY_TOKEN", "value_last4": "cret", "deploy_readable": true, "updated_at": "2026-01-01T00:00:00Z"},
		})
	}))
	defer srv.Close()
	writeAPIConfig(t, srv.URL)

	out := &bytes.Buffer{}
	iio := &issueIO{Stdout: out, Stderr: io.Discard}
	if err := secretList(iio, secretArgs(t, "list", "--project", "proj1")); err != nil {
		t.Fatalf("list: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "MY_TOKEN") || !strings.Contains(s, "cret") {
		t.Fatalf("missing expected fields:\n%s", s)
	}
	if strings.Contains(s, "s3cr3t-full-value") {
		t.Fatal("list must never render plaintext")
	}
}

func TestSecretDelete_ResolvesNameToID(t *testing.T) {
	var deletedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "sec-1", "name": "MY_TOKEN"}})
		case r.Method == http.MethodDelete:
			deletedPath = r.URL.Path
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	writeAPIConfig(t, srv.URL)

	out := &bytes.Buffer{}
	iio := &issueIO{Stdout: out, Stderr: io.Discard}
	if err := secretDelete(iio, secretArgs(t, "delete", "--project", "proj1", "MY_TOKEN")); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if deletedPath != "/api/projects/proj1/secrets/sec-1" {
		t.Fatalf("unexpected delete path: %q", deletedPath)
	}
}

func TestSecretCommand_RegisteredUnderRoot(t *testing.T) {
	root := rootCommand()
	for _, c := range root.Sub {
		if c.Name == "secret" {
			return
		}
	}
	t.Fatal("secret command not registered under rootCommand()")
}
