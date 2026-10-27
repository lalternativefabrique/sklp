package report

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/restore/drill"
)

func sample() drill.Report {
	return drill.Report{
		Space: "app-production", Engine: "postgres", Target: "app", Status: drill.StatusOK,
		Backup:    drill.Backup{Label: "20260926-030002D"},
		Inventory: drill.Inventory{Position: "1/67000A0", Collections: []drill.Collection{{Group: "app", Name: "public.users", Count: 3}}},
	}
}

func TestSendKeepsTheFieldsHealthAlreadyReads(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/backup-checks" || r.Header.Get("Authorization") != "Bearer k" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	if err := (Client{Endpoint: srv.URL + "/", APIKey: "k"}).Send(context.Background(), sample()); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]any{"space": "app-production", "stanza": "app", "kind": "pgbackrest-restore", "status": "ok", "backup_label": "20260926-030002D", "restored_lsn": "1/67000A0", "tables": float64(1)} {
		if got[k] != want {
			t.Errorf("%s = %v, want %v", k, got[k], want)
		}
	}
	if rep, ok := got["report"].(map[string]any); !ok || rep["engine"] != "postgres" {
		t.Errorf("the full report is missing: %v", got["report"])
	}
}

func TestSendRetriesAServerErrorButNotARejection(t *testing.T) {
	var calls atomic.Int32
	code := http.StatusBadGateway
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(code)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	c := Client{Endpoint: srv.URL, APIKey: "k", Attempts: 3, Backoff: time.Millisecond}
	if err := c.Send(context.Background(), sample()); err != nil || calls.Load() != 3 {
		t.Fatalf("err=%v calls=%d, want success on the third attempt", err, calls.Load())
	}

	calls.Store(0)
	code = http.StatusBadRequest
	if err := c.Send(context.Background(), sample()); err == nil || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d, want one attempt for a 400", err, calls.Load())
	}
}
