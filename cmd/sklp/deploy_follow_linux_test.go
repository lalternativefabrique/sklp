package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lalternativefabrique/sklp/spacemanager/control"
)

func TestDeployFollowCommandsReachTheNodeRoutes(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var posted control.FollowRequest
	var deleted string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == control.RouteFollows:
			_ = json.NewDecoder(r.Body).Decode(&posted)
			if posted.Repo == "https://github.com/x/taken" {
				http.Error(w, "follows: follow conflict: a followed repo already has that name", http.StatusConflict)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(control.FollowEntry{Name: "blog", Repo: posted.Repo, Branch: "main"})
		case r.Method == http.MethodGet && r.URL.Path == control.RouteFollows:
			_ = json.NewEncoder(w).Encode(control.FollowsResponse{Node: "n1", Follows: []control.FollowEntry{{Name: "blog", Repo: "https://github.com/moi/blog", Branch: "main"}}})
		case r.Method == http.MethodDelete && r.URL.Path == control.RouteFollows+"/blog":
			deleted = "blog"
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	sum := sha256.Sum256(srv.Certificate().Raw)
	nf := &netFlags{server: srv.URL, token: "tok", caFinger: hex.EncodeToString(sum[:])}
	ctx := context.Background()

	if code := runDeployFollow(ctx, nf, control.FollowRequest{Repo: "https://github.com/moi/blog", Branch: "dev"}); code != 0 {
		t.Fatalf("follow exit %d", code)
	}
	if posted.Repo != "https://github.com/moi/blog" || posted.Branch != "dev" {
		t.Fatalf("posted %+v", posted)
	}
	if code := runDeployFollow(ctx, nf, control.FollowRequest{Repo: "https://github.com/x/taken"}); code != 1 {
		t.Fatalf("a refused follow must exit 1, got %d", code)
	}
	if code := runDeployFollow(ctx, nf, control.FollowRequest{}); code != 2 {
		t.Fatalf("no repo must be a usage error, got %d", code)
	}
	if code := runDeployFollows(ctx, nf, true); code != 0 {
		t.Fatalf("follows exit %d", code)
	}
	if code := runDeployUnfollow(ctx, nf, "blog"); code != 0 || deleted != "blog" {
		t.Fatalf("unfollow exit %d deleted=%q", code, deleted)
	}
	if code := runDeployUnfollow(ctx, nf, "ghost"); code != 1 {
		t.Fatalf("unfollowing an unknown name must exit 1, got %d", code)
	}
}
