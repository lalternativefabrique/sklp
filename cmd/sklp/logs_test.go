package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/internal/cli"
)

// logsArgs parses argv against the real `sklp logs` flag set, so the tests
// exercise the declared flags rather than a hand-built stub.
func logsArgs(t *testing.T, argv ...string) cli.Args {
	t.Helper()
	a, err := cli.ParseLeaf(logsCommand(), argv)
	if err != nil {
		t.Fatalf("parse logs %v: %v", argv, err)
	}
	return a
}

func sampleLogPage(hasMore bool) map[string]any {
	t1, _ := time.Parse(time.RFC3339, "2026-07-31T08:47:21Z")
	t2, _ := time.Parse(time.RFC3339, "2026-07-31T08:47:23Z")
	return map[string]any{
		"items": []map[string]any{
			{"timestamp": t1, "severity": "ERROR", "service": "svc-a", "project_name": "api-gateway", "project_id": "p1", "body": "upstream timeout after 30s"},
			{"timestamp": t2, "severity": "ERROR", "service": "svc-b", "project_name": "worker", "project_id": "p2", "body": "job 41f2 failed"},
		},
		"next_cursor": "cur-42",
		"has_more":    hasMore,
	}
}

// logsServer serves the stack-logs endpoint plus the project lookup used for
// stack derivation, recording the query the command actually sent.
func logsServer(t *testing.T, stackID string, page map[string]any, gotQuery *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/projects/p1":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "p1", "stack_id": stackID})
		case r.URL.Path == "/api/stacks/"+stackID+"/logs":
			if gotQuery != nil {
				*gotQuery = r.URL.RawQuery
			}
			_ = json.NewEncoder(w).Encode(page)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestLogs_DerivesStackFromProject(t *testing.T) {
	var query string
	srv := logsServer(t, "stack-9", sampleLogPage(false), &query)
	defer srv.Close()
	writeAPIConfig(t, srv.URL)

	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	lio := &logsIO{Stdout: out, Stderr: errOut}
	if err := runLogs(context.Background(), lio, logsArgs(t, "--project", "p1", "--severity", "ERROR")); err != nil {
		t.Fatalf("runLogs: %v", err)
	}

	if !strings.Contains(query, "severity=ERROR") {
		t.Errorf("severity not forwarded, query=%q", query)
	}
	if !strings.Contains(query, "project_ids=p1") {
		t.Errorf("project_ids not forwarded, query=%q", query)
	}
	got := out.String()
	for _, want := range []string{"api-gateway", "upstream timeout after 30s", "worker"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

// An explicit --stack must not trigger the project lookup: a server that 404s
// /api/projects/* proves derivation was skipped.
func TestLogs_ExplicitStackSkipsDerivation(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/stacks/stack-explicit/logs" {
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
			return
		}
		query = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(sampleLogPage(false))
	}))
	defer srv.Close()
	writeAPIConfig(t, srv.URL)

	lio := &logsIO{Stdout: io.Discard, Stderr: io.Discard}
	if err := runLogs(context.Background(), lio, logsArgs(t, "--stack", "stack-explicit")); err != nil {
		t.Fatalf("runLogs: %v", err)
	}
	if strings.Contains(query, "project_ids") {
		t.Errorf("no project_ids expected, query=%q", query)
	}
}

func TestLogs_MultipleProjectsForwarded(t *testing.T) {
	var query string
	srv := logsServer(t, "stack-9", sampleLogPage(false), &query)
	defer srv.Close()
	writeAPIConfig(t, srv.URL)

	lio := &logsIO{Stdout: io.Discard, Stderr: io.Discard}
	if err := runLogs(context.Background(), lio, logsArgs(t, "--project", "p1, p2 ,")); err != nil {
		t.Fatalf("runLogs: %v", err)
	}
	if !strings.Contains(query, "project_ids=p1%2Cp2") {
		t.Errorf("want both ids, got query=%q", query)
	}
}

func TestLogs_NoTargetIsAnError(t *testing.T) {
	srv := logsServer(t, "stack-9", sampleLogPage(false), nil)
	defer srv.Close()
	writeAPIConfig(t, srv.URL) // clears SKLP_PROJECT

	lio := &logsIO{Stdout: io.Discard, Stderr: io.Discard}
	err := runLogs(context.Background(), lio, logsArgs(t))
	if err == nil || !strings.Contains(err.Error(), "--project") {
		t.Fatalf("want a target error, got %v", err)
	}
}

// Truncation must be announced, and on stderr — stdout stays pipeable.
func TestLogs_TruncationNoticeOnStderr(t *testing.T) {
	srv := logsServer(t, "stack-9", sampleLogPage(true), nil)
	defer srv.Close()
	writeAPIConfig(t, srv.URL)

	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	if err := runLogs(context.Background(), &logsIO{Stdout: out, Stderr: errOut}, logsArgs(t, "--project", "p1")); err != nil {
		t.Fatalf("runLogs: %v", err)
	}
	if !strings.Contains(errOut.String(), "cur-42") {
		t.Errorf("want cursor in notice, got %q", errOut.String())
	}
	if strings.Contains(out.String(), "more available") {
		t.Errorf("notice leaked into stdout:\n%s", out.String())
	}
}

// An empty result must be distinguishable from a broken query.
func TestLogs_EmptyResultNotice(t *testing.T) {
	empty := map[string]any{"items": []map[string]any{}, "has_more": false}
	srv := logsServer(t, "stack-9", empty, nil)
	defer srv.Close()
	writeAPIConfig(t, srv.URL)

	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	if err := runLogs(context.Background(), &logsIO{Stdout: out, Stderr: errOut}, logsArgs(t, "--project", "p1")); err != nil {
		t.Fatalf("runLogs: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("stdout must stay empty, got %q", out.String())
	}
	if !strings.Contains(errOut.String(), "no logs in range") {
		t.Errorf("want empty notice, got %q", errOut.String())
	}
}

func TestLogs_JSONPassthrough(t *testing.T) {
	srv := logsServer(t, "stack-9", sampleLogPage(false), nil)
	defer srv.Close()
	writeAPIConfig(t, srv.URL)

	out := &bytes.Buffer{}
	if err := runLogs(context.Background(), &logsIO{Stdout: out, Stderr: io.Discard}, logsArgs(t, "--project", "p1", "--json")); err != nil {
		t.Fatalf("runLogs: %v", err)
	}
	var page stackLogPage
	if err := json.Unmarshal(out.Bytes(), &page); err != nil {
		t.Fatalf("stdout is not the raw payload: %v", err)
	}
	if page.NextCursor != "cur-42" || len(page.Items) != 2 {
		t.Errorf("payload altered: %+v", page)
	}
}

func TestBuildLogsQuery_TimeRange(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2026-07-31T10:00:00Z")

	tests := []struct {
		name     string
		argv     []string
		wantFrom string
		wantErr  string
	}{
		{name: "default is one hour", argv: nil, wantFrom: "2026-07-31T09:00:00Z"},
		{name: "since is relative to now", argv: []string{"--since", "15m"}, wantFrom: "2026-07-31T09:45:00Z"},
		{name: "from is passed through", argv: []string{"--from", "2026-07-30T00:00:00Z"}, wantFrom: "2026-07-30T00:00:00Z"},
		{name: "since and from conflict", argv: []string{"--since", "1h", "--from", "2026-07-30T00:00:00Z"}, wantErr: "mutually exclusive"},
		{name: "bad since", argv: []string{"--since", "yesterday"}, wantErr: "invalid --since"},
		{name: "negative since", argv: []string{"--since", "-5m"}, wantErr: "must be positive"},
		{name: "bad from", argv: []string{"--from", "31/07/2026"}, wantErr: "invalid --from"},
		{name: "bad to", argv: []string{"--to", "soon"}, wantErr: "invalid --to"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q, err := buildLogsQuery(logsArgs(t, tc.argv...), now)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want error %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildLogsQuery: %v", err)
			}
			if got := q.Get("from"); got != tc.wantFrom {
				t.Errorf("from = %q, want %q", got, tc.wantFrom)
			}
			if got := q.Get("limit"); got != "100" {
				t.Errorf("default limit = %q, want 100", got)
			}
		})
	}
}

func TestLogOrigin(t *testing.T) {
	tests := []struct {
		name string
		item stackLogItem
		want string
	}{
		{name: "both kept", item: stackLogItem{ProjectName: "Acme", Service: "acme-core"}, want: "Acme/acme-core"},
		{name: "redundant service dropped", item: stackLogItem{ProjectName: "worker", Service: "Worker"}, want: "worker"},
		{name: "service only", item: stackLogItem{Service: "svc-a"}, want: "svc-a"},
		{name: "project only", item: stackLogItem{ProjectName: "api"}, want: "api"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := logOrigin(tc.item); got != tc.want {
				t.Errorf("logOrigin = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTruncateMessage(t *testing.T) {
	if got := truncateMessage("  multi\nline\r\nmsg  "); got != "multi line msg" {
		t.Errorf("newlines not collapsed: %q", got)
	}
	long := strings.Repeat("é", maxMessageWidth+20)
	got := truncateMessage(long)
	if r := []rune(got); len(r) != maxMessageWidth+1 || r[len(r)-1] != '…' {
		t.Errorf("want %d runes + ellipsis, got %d", maxMessageWidth, len(r))
	}
}

// samplePage decodes sampleLogPage into the typed shape renderLogs consumes, so
// the render tests share the same fixture as the HTTP-level ones.
func samplePage(t *testing.T) stackLogPage {
	t.Helper()
	raw, err := json.Marshal(sampleLogPage(false))
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	var page stackLogPage
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	return page
}

// Colour is opt-in per logsIO: the default zero value stays plain, which is what
// keeps `sklp logs | jq` and an AI agent's stdout free of escape codes.
func TestRenderLogs_NoColorByDefault(t *testing.T) {
	out := &bytes.Buffer{}
	lio := &logsIO{Stdout: out, Stderr: io.Discard}
	if err := renderLogs(lio, samplePage(t)); err != nil {
		t.Fatalf("renderLogs: %v", err)
	}
	if strings.Contains(out.String(), "\033[") {
		t.Errorf("plain output must carry no ANSI:\n%q", out.String())
	}
}

func TestRenderLogs_ColorWhenEnabled(t *testing.T) {
	out := &bytes.Buffer{}
	lio := &logsIO{Stdout: out, Stderr: io.Discard, Color: true}
	if err := renderLogs(lio, samplePage(t)); err != nil {
		t.Fatalf("renderLogs: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, ansiRed) {
		t.Errorf("ERROR severity must be red:\n%q", got)
	}
	if !strings.Contains(got, ansiReset) {
		t.Errorf("colour must be reset:\n%q", got)
	}
	// The message itself keeps the default foreground — it is the information.
	if !strings.Contains(got, "upstream timeout after 30s") {
		t.Errorf("message missing:\n%q", got)
	}
}

// Padding must happen before colouring: %-6s counts bytes, so wrapping the
// severity first would break column alignment.
func TestRenderLogs_ColumnWidthSurvivesColor(t *testing.T) {
	page := stackLogPage{Items: []stackLogItem{
		{Timestamp: time.Unix(0, 0).UTC(), Severity: "WARN", ProjectName: "p", Body: "m"},
	}}
	out := &bytes.Buffer{}
	if err := renderLogs(&logsIO{Stdout: out, Stderr: io.Discard, Color: true}, page); err != nil {
		t.Fatalf("renderLogs: %v", err)
	}
	// WARN padded to 6 => "WARN  " must sit inside the colour wrapper.
	if !strings.Contains(out.String(), ansiYellow+"WARN  "+ansiReset) {
		t.Errorf("severity must be padded before colouring:\n%q", out.String())
	}
}

func TestSeverityColor(t *testing.T) {
	tests := map[string]string{
		"ERROR": ansiRed, "fatal": ansiRed, "WARN": ansiYellow,
		"warning": ansiYellow, "DEBUG": ansiDim, "INFO": "", "": "",
	}
	for sev, want := range tests {
		if got := severityColor(sev); got != want {
			t.Errorf("severityColor(%q) = %q, want %q", sev, got, want)
		}
	}
}

// $NO_COLOR and --no-color must both win over a real terminal.
func TestWantColor(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	if wantColor(true, os.Stdout) {
		t.Error("--no-color must disable colour")
	}
	// A pipe (not a char device) must never be coloured.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()
	if wantColor(false, w) {
		t.Error("a pipe must not be coloured")
	}
	if wantColor(false, nil) {
		t.Error("nil file must not be coloured")
	}

	t.Setenv("NO_COLOR", "1")
	if wantColor(false, os.Stdout) {
		t.Error("$NO_COLOR must disable colour")
	}
}
