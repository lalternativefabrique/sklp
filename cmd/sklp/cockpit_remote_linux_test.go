//go:build linux

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/internal/config"
	"github.com/lalternativefabrique/sklp/space/spaceapi"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
)

// A node that ANSWERS must have its spaces rendered under its context name.
func TestReadNodeHappyPath(t *testing.T) {
	const token = "probe-token"
	body, _ := json.Marshal([]spaceapi.SpaceInfo{
		{Name: "production", Status: "running", Supervised: true,
			Containers: []spaceapi.ContainerInfo{{ID: "web-0", Status: "running"}},
			Volumes:    []string{"pgdata"}},
	})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != control.RouteSpaces {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(control.SpacesResponse{Node: "vm-1", Spaces: body})
	}))
	defer srv.Close()

	sum := sha256.Sum256(srv.Certificate().Raw)
	node := readNode(context.Background(), "acme-vm", config.Context{
		Server:        srv.URL,
		Token:         token,
		CAFingerprint: hex.EncodeToString(sum[:]),
	})
	if node.Err != nil {
		t.Fatalf("err = %v", node.Err)
	}
	if len(node.Spaces) != 1 {
		t.Fatalf("spaces = %+v", node.Spaces)
	}
	sp := node.Spaces[0]
	if sp.Name != "production" || len(sp.Runc) != 1 || sp.Runc[0].ID != "web-0" {
		t.Fatalf("space not translated: %+v", sp)
	}
	if len(sp.Volumes) != 1 || !sp.Supervised {
		t.Fatalf("volumes/supervised lost in translation: %+v", sp)
	}
}

// A node-level failure must be described as such: the client phrases its
// transport errors around a single space, which says nothing true about a
// machine that was asked for its whole inventory.
func TestNodeReadErrIsAboutTheNode(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   error
		want string
	}{
		{"old node", control.ErrNoInventory, "upgrade"},
		{"nothing listening", control.ErrNoSupervisor, "sklet running"},
		{"timeout", context.DeadlineExceeded, "unreachable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := nodeReadErr(fmt.Errorf("wrapped: %w", tc.in)).Error()
			if !strings.Contains(got, tc.want) {
				t.Fatalf("err = %q, want it to mention %q", got, tc.want)
			}
			if strings.Contains(got, "space \"\"") {
				t.Fatalf("err = %q still talks about an unnamed space", got)
			}
		})
	}
}

// A node dispatches every space it hosts under SpaceRoutePrefix, so the space a
// context happens to name does not limit what can be streamed: any space of that
// node is reachable, and the client is scoped to the one being read.
func TestLogClientReachesEverySpaceOfANode(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SKLP_CONFIG", dir+"/config.yaml")
	cfg, path, err := config.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	node := multiSpaceNode(t)
	cfg.SetContext("acme-vm", config.Context{
		Server: node.URL, Token: "tok", Space: "production", CAFingerprint: certFingerprint(node),
	})
	if err := cfg.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	for _, space := range []string{"production", "shop-production"} {
		client, err := logClient("acme-vm", space)
		if err != nil {
			t.Fatalf("logClient(%q): %v", space, err)
		}
		var buf bytes.Buffer
		if err := client.Logs(context.Background(), "core", false, &buf); err != nil {
			t.Fatalf("logs for %q: %v", space, err)
		}
		if got := buf.String(); got != space+"/core" {
			t.Errorf("streamed %q, want the log served on %q's own routes", got, space)
		}
	}
}

// multiSpaceNode is a stand-in for a node running ServeNetMulti: it answers a
// log request only on that space's SpaceRoutePrefix, echoing "<space>/<svc>" so
// a test can tell WHICH space's routes the request landed on. A bare (single-
// space) path 404s, exactly as a multi-space node's dispatcher does.
func multiSpaceNode(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const spaces = "/v1/spaces/"
		rest, ok := strings.CutPrefix(r.URL.Path, spaces)
		if !ok {
			http.NotFound(w, r)
			return
		}
		space, tail, ok := strings.Cut(rest, "/logs/")
		if !ok || space == "" || tail == "" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, space+"/"+tail)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// certFingerprint is the SHA-256 of the test server's cert, the pin the client
// verifies instead of a system trust store.
func certFingerprint(srv *httptest.Server) string {
	sum := sha256.Sum256(srv.Certificate().Raw)
	return hex.EncodeToString(sum[:])
}
