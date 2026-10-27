package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/internal/config"
)

// issueArgs parses argv against the named issue sub-verb's declared flags +
// positionals, returning the cli.Args the verb reads. It mirrors how the façade
// would dispatch the sub-verb, so the parsing tests exercise the real flags.
func issueArgs(t *testing.T, sub string, argv ...string) cli.Args {
	t.Helper()
	cmd := issueCommand()
	for _, c := range cmd.Sub {
		if c.Name == sub {
			a, err := cli.ParseLeaf(c, argv)
			if err != nil {
				t.Fatalf("parse issue %s %v: %v", sub, argv, err)
			}
			return a
		}
	}
	t.Fatalf("unknown issue sub-verb %q", sub)
	return nil
}

// writeAPIConfig pins XDG to a temp dir and stores an api block pointing at
// the given API URL with a known PAT-shaped token. It deliberately also
// records a deploy context: `sklp issue` must resolve the SaaS endpoint from
// the api block alone, whatever the deploy selector says.
func writeAPIConfig(t *testing.T, apiURL string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv(config.EnvContext, "")
	t.Setenv(config.EnvToken, "")
	t.Setenv(config.EnvContextToken, "")
	t.Setenv("SKLP_PROJECT", "")
	cfg := &config.Config{
		API:            config.API{URL: apiURL, Token: "sklp_pat_abc_secret"},
		CurrentContext: "prod-node",
		Contexts: map[string]config.Context{
			"prod-node": {Server: "https://vm.example:9443", Token: "node-bearer"},
		},
	}
	path, err := config.Path()
	if err != nil {
		t.Fatalf("path: %v", err)
	}
	if err := cfg.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}
}

func sampleIssues() []map[string]any {
	t1, _ := time.Parse(time.RFC3339, "2026-01-02T12:00:00Z")
	t2, _ := time.Parse(time.RFC3339, "2026-01-03T12:00:00Z")
	t3, _ := time.Parse(time.RFC3339, "2026-01-04T12:00:00Z")
	return []map[string]any{
		{"id": "id-aaaaaaaa-1111", "project_id": "p1", "type": "bug", "status": "backlog", "title": "alpha bug", "created_at": t1, "updated_at": t1},
		{"id": "id-bbbbbbbb-2222", "project_id": "p1", "type": "feature", "status": "done", "title": "beta feat", "created_at": t2, "updated_at": t2},
		{"id": "id-cccccccc-3333", "project_id": "p1", "type": "bug", "status": "in_progress", "title": "gamma bug fix", "created_at": t3, "updated_at": t3},
	}
}

func TestIssueList_Table(t *testing.T) {
	items := sampleIssues()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/projects/proj1/issues" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer sklp_pat_") {
			w.WriteHeader(401)
			return
		}
		_ = json.NewEncoder(w).Encode(items)
	}))
	defer srv.Close()
	writeAPIConfig(t, srv.URL)

	out := &bytes.Buffer{}
	iio := &issueIO{Stdout: out, Stderr: io.Discard}
	if err := issueList(iio, issueArgs(t, "list", "--project", "proj1")); err != nil {
		t.Fatalf("list: %v", err)
	}
	s := out.String()
	for _, want := range []string{"id-aaaaa", "id-bbbbb", "id-ccccc", "alpha bug", "beta feat", "gamma bug fix", "TITLE", "TRACKER"} {
		if !strings.Contains(s, want) {
			t.Fatalf("output missing %q:\n%s", want, s)
		}
	}
}

func TestIssueList_StatusQueryParamSent(t *testing.T) {
	var sawStatus string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawStatus = r.URL.Query().Get("status")
		_ = json.NewEncoder(w).Encode([]any{})
	}))
	defer srv.Close()
	writeAPIConfig(t, srv.URL)

	out := &bytes.Buffer{}
	iio := &issueIO{Stdout: out, Stderr: io.Discard}
	if err := issueList(iio, issueArgs(t, "list", "--project", "p1", "--status", "backlog")); err != nil {
		t.Fatalf("list: %v", err)
	}
	if sawStatus != "backlog" {
		t.Fatalf("expected ?status=backlog, got %q", sawStatus)
	}
}

func TestIssueList_TypeClientSide(t *testing.T) {
	items := sampleIssues()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(items)
	}))
	defer srv.Close()
	writeAPIConfig(t, srv.URL)

	out := &bytes.Buffer{}
	iio := &issueIO{Stdout: out, Stderr: io.Discard}
	if err := issueList(iio, issueArgs(t, "list", "--project", "p1", "--type", "feature")); err != nil {
		t.Fatalf("list: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "beta feat") {
		t.Fatalf("expected feature row: %s", s)
	}
	if strings.Contains(s, "alpha bug") || strings.Contains(s, "gamma bug fix") {
		t.Fatalf("type filter leaked rows: %s", s)
	}
}

func TestIssueList_JSON(t *testing.T) {
	items := sampleIssues()
	raw, _ := json.Marshal(items)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(raw)
	}))
	defer srv.Close()
	writeAPIConfig(t, srv.URL)

	out := &bytes.Buffer{}
	iio := &issueIO{Stdout: out, Stderr: io.Discard}
	if err := issueList(iio, issueArgs(t, "list", "--project", "p1", "--json")); err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out.String(), `"id-aaaaaaaa-1111"`) {
		t.Fatalf("json passthrough missing payload: %s", out.String())
	}
}

func TestIssueList_Plain(t *testing.T) {
	items := sampleIssues()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(items)
	}))
	defer srv.Close()
	writeAPIConfig(t, srv.URL)

	out := &bytes.Buffer{}
	iio := &issueIO{Stdout: out, Stderr: io.Discard}
	if err := issueList(iio, issueArgs(t, "list", "--project", "p1", "--plain")); err != nil {
		t.Fatalf("list: %v", err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d:\n%s", len(lines), out.String())
	}
	for _, ln := range lines {
		// id, type, status, project, title, tracker, created → 6 tabs
		if strings.Count(ln, "\t") < 5 {
			t.Fatalf("expected tab-separated columns: %q", ln)
		}
	}
}

func TestIssueNew_PostsJSON(t *testing.T) {
	var seenBody map[string]string
	var seenPath, seenCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenCT = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&seenBody)
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"id":"new-issue-xyz"}`))
	}))
	defer srv.Close()
	writeAPIConfig(t, srv.URL)

	out := &bytes.Buffer{}
	iio := &issueIO{Stdout: out, Stderr: io.Discard}
	if err := issueNew(iio, issueArgs(t, "new", "--project", "p1", "--type", "bug", "--body", "details", "broken thing")); err != nil {
		t.Fatalf("new: %v", err)
	}
	if seenPath != "/api/projects/p1/issues" {
		t.Fatalf("wrong path: %s", seenPath)
	}
	if !strings.HasPrefix(seenCT, "application/json") {
		t.Fatalf("wrong content-type: %s", seenCT)
	}
	if seenBody["title"] != "broken thing" || seenBody["type"] != "bug" || seenBody["body"] != "details" {
		t.Fatalf("bad body: %#v", seenBody)
	}
	if !strings.Contains(out.String(), "new-issue-xyz") {
		t.Fatalf("stdout missing id: %s", out.String())
	}
}

func TestIssueNew_Multipart(t *testing.T) {
	var seenCT string
	var sawTitle, sawType string
	var sawAttachFile string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenCT = r.Header.Get("Content-Type")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
		}
		sawTitle = r.FormValue("title")
		sawType = r.FormValue("type")
		if r.MultipartForm != nil && len(r.MultipartForm.File["attachments"]) > 0 {
			sawAttachFile = r.MultipartForm.File["attachments"][0].Filename
		}
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"id":"i-1"}`))
	}))
	defer srv.Close()
	writeAPIConfig(t, srv.URL)

	// Create a small temp file to attach.
	dir := t.TempDir()
	attach := dir + "/note.txt"
	if err := writeFile(attach, "hello attach"); err != nil {
		t.Fatalf("write file: %v", err)
	}

	out := &bytes.Buffer{}
	iio := &issueIO{Stdout: out, Stderr: io.Discard}
	if err := issueNew(iio, issueArgs(t, "new", "--project", "p1", "--type", "task", "--attach", attach, "with file")); err != nil {
		t.Fatalf("new: %v", err)
	}
	if !strings.HasPrefix(seenCT, "multipart/form-data") {
		t.Fatalf("expected multipart content-type, got %s", seenCT)
	}
	if sawTitle != "with file" || sawType != "task" {
		t.Fatalf("bad form fields: title=%q type=%q", sawTitle, sawType)
	}
	if sawAttachFile != "note.txt" {
		t.Fatalf("attachment filename: %q", sawAttachFile)
	}
}

func TestIssueNew_NoProject(t *testing.T) {
	writeAPIConfig(t, "http://127.0.0.1:1")
	iio := &issueIO{Stdout: io.Discard, Stderr: io.Discard}
	err := issueNew(iio, issueArgs(t, "new", "hello"))
	if err == nil || !strings.Contains(err.Error(), "specify --project") {
		t.Fatalf("expected project error, got %v", err)
	}
}

func TestIssueList_401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()
	writeAPIConfig(t, srv.URL)

	iio := &issueIO{Stdout: io.Discard, Stderr: io.Discard}
	err := issueList(iio, issueArgs(t, "list", "--project", "p1"))
	if err == nil || !strings.Contains(err.Error(), "token rejected") {
		t.Fatalf("expected token rejected hint, got %v", err)
	}
}

func TestIssueShow_FiltersByID(t *testing.T) {
	items := sampleIssues()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(items)
	}))
	defer srv.Close()
	writeAPIConfig(t, srv.URL)

	out := &bytes.Buffer{}
	iio := &issueIO{Stdout: out, Stderr: io.Discard}
	if err := issueShow(iio, issueArgs(t, "show", "--project", "p1", "id-bbbbbbbb-2222")); err != nil {
		t.Fatalf("show: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "id-bbbbbbbb-2222") || !strings.Contains(s, "beta feat") {
		t.Fatalf("show output wrong: %s", s)
	}
}

func TestIssueStatus_Posts(t *testing.T) {
	var seenPath string
	var seenBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&seenBody)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"i-1","status":"done"}`))
	}))
	defer srv.Close()
	writeAPIConfig(t, srv.URL)

	out := &bytes.Buffer{}
	iio := &issueIO{Stdout: out, Stderr: io.Discard}
	if err := issueStatus(iio, issueArgs(t, "status", "--project", "p1", "--reason", "fixed", "i-1", "done")); err != nil {
		t.Fatalf("status: %v", err)
	}
	if seenPath != "/api/projects/p1/issues/i-1/status" {
		t.Fatalf("wrong path: %s", seenPath)
	}
	if seenBody["status"] != "done" || seenBody["closed_reason"] != "fixed" {
		t.Fatalf("bad body: %#v", seenBody)
	}
}

func TestIssueEdit_PatchesOnlyProvidedFields(t *testing.T) {
	var seenMethod, seenPath string
	var seenBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenMethod = r.Method
		seenPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&seenBody)
		_, _ = w.Write([]byte(`{"id":"i-1"}`))
	}))
	defer srv.Close()
	writeAPIConfig(t, srv.URL)

	out := &bytes.Buffer{}
	iio := &issueIO{Stdout: out, Stderr: io.Discard}
	if err := issueEdit(iio, issueArgs(t, "edit", "--project", "p1", "--title", "new", "i-1")); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if seenMethod != http.MethodPatch {
		t.Fatalf("expected PATCH, got %s", seenMethod)
	}
	if seenPath != "/api/projects/p1/issues/i-1" {
		t.Fatalf("wrong path: %s", seenPath)
	}
	if _, ok := seenBody["body"]; ok {
		t.Fatalf("body field should be absent: %#v", seenBody)
	}
	if seenBody["title"] != "new" {
		t.Fatalf("bad title: %#v", seenBody)
	}
}

func TestIssueEdit_RequiresAtLeastOneField(t *testing.T) {
	writeAPIConfig(t, "http://127.0.0.1:1")
	iio := &issueIO{Stdout: io.Discard, Stderr: io.Discard}
	err := issueEdit(iio, issueArgs(t, "edit", "--project", "p1", "i-1"))
	if err == nil || !strings.Contains(err.Error(), "at least one of") {
		t.Fatalf("expected required-field error, got %v", err)
	}
}

func TestIssuePromote_Posts(t *testing.T) {
	var seenPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"id":"i-2"}`))
	}))
	defer srv.Close()
	writeAPIConfig(t, srv.URL)

	out := &bytes.Buffer{}
	iio := &issueIO{Stdout: out, Stderr: io.Discard}
	if err := issuePromote(iio, issueArgs(t, "promote", "--project", "p1", "f-1")); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if seenPath != "/api/projects/p1/feedback/f-1/promote" {
		t.Fatalf("wrong path: %s", seenPath)
	}
}

func TestIssueGhUnlink_Deletes(t *testing.T) {
	var seenMethod, seenPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenMethod = r.Method
		seenPath = r.URL.Path
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"i-1"}`))
	}))
	defer srv.Close()
	writeAPIConfig(t, srv.URL)

	out := &bytes.Buffer{}
	iio := &issueIO{Stdout: out, Stderr: io.Discard}
	if err := issueGhUnlink(iio, issueArgs(t, "gh-unlink", "--project", "p1", "i-1")); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	if seenMethod != http.MethodDelete {
		t.Fatalf("expected DELETE, got %s", seenMethod)
	}
	if seenPath != "/api/projects/p1/issues/i-1/tracker" {
		t.Fatalf("wrong path: %s", seenPath)
	}
}

func TestIssueGhTracker_PostsClose(t *testing.T) {
	var seenPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"i-1"}`))
	}))
	defer srv.Close()
	writeAPIConfig(t, srv.URL)

	out := &bytes.Buffer{}
	iio := &issueIO{Stdout: out, Stderr: io.Discard}
	if err := issueGhTracker(iio, issueArgs(t, "gh-close", "--project", "p1", "i-1"), "/close"); err != nil {
		t.Fatalf("gh-close: %v", err)
	}
	if seenPath != "/api/projects/p1/issues/i-1/tracker/github/close" {
		t.Fatalf("wrong path: %s", seenPath)
	}
}

// writeFile is a tiny helper used in multipart tests.
func writeFile(path, contents string) error {
	return os.WriteFile(path, []byte(contents), 0o600)
}
