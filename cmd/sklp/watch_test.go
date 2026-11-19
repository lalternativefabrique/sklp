package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/internal/cli"
)

func watchTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/stacks":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "st-1", "name": "LAlter"}})
		case "/api/stacks/st-1/image-watches":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": "w-1", "name": "messag-core", "status": "failed", "enabled": true, "current_tag": "20261118-0913-92b12caafea6", "held_tag": "20261117-1752-0248d022d20c"},
			})
		case "/api/stacks/st-1/image-watches/w-1/rolls":
			if r.URL.Query().Get("limit") != "5" {
				t.Errorf("limit = %q, want 5", r.URL.Query().Get("limit"))
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"from_tag": "a", "to_tag": "b", "result": "failed", "error_message": "rollout outcome not observed within 8m0s", "created_at": "2026-11-18T09:15:00Z"},
			})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func runWatch(t *testing.T, argv ...string) string {
	t.Helper()
	srv := watchTestServer(t)
	writeAPIConfig(t, srv.URL)
	client, err := resolveClient()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	args, err := cli.ParseLeaf(findFacadeSub(t, watchCommand(), argv[0]), argv[1:])
	if err != nil {
		t.Fatal(err)
	}
	switch argv[0] {
	case "ls":
		err = watchList(context.Background(), &out, client, args)
	case "rolls":
		err = watchRolls(context.Background(), &out, client, args)
	}
	if err != nil {
		t.Fatalf("%s: %v", argv[0], err)
	}
	return out.String()
}

func TestWatchListShowsHeldTagByStackName(t *testing.T) {
	got := runWatch(t, "ls", "--stack", "lalter")
	for _, want := range []string{"messag-core", "failed", "20261118-0913-92b12caafea6", "20261117-1752-0248d022d20c"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
}

func TestWatchRollsShowsTheRollError(t *testing.T) {
	got := runWatch(t, "rolls", "messag-core", "--stack", "st-1", "--limit", "5")
	if !strings.Contains(got, "rollout outcome not observed within 8m0s") {
		t.Errorf("output lacks the roll error:\n%s", got)
	}
}
