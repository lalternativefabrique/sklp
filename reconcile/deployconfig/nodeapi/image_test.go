package nodeapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/reconcile/imageupdate/domain"
)

func TestControlRollerRollsAndReadsRollouts(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody imageRequest
	result := "configured"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/image"):
			gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			_ = json.NewEncoder(w).Encode(applyResponse{Result: result})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/spaces/shop/state":
			_, _ = w.Write([]byte(`{"services":[{"name":"web","image":"rg.example.com/shop/web:2","rollouts":[{"id":"r1","to_image":"rg.example.com/shop/web:2","result":"succeeded"}]}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	sum := sha256.Sum256(srv.Certificate().Raw)

	w := &domain.Watch{
		Service: "web", ImageRepo: "rg.example.com/shop/web",
		NodeServer: srv.URL, NodeToken: "tok", NodeSpace: "shop",
		NodeCAFingerprint: hex.EncodeToString(sum[:]),
	}
	r := NewControlRoller()

	got, err := r.Roll(context.Background(), w, "2")
	if err != nil || got != domain.RollResultApplied {
		t.Fatalf("Roll = %q, %v; want applied", got, err)
	}
	if gotPath != "/v1/spaces/shop/image" || gotAuth != "Bearer tok" {
		t.Fatalf("hit %s with %q", gotPath, gotAuth)
	}
	if gotBody.Service != "web" || gotBody.Image != "rg.example.com/shop/web:2" {
		t.Fatalf("body = %+v", gotBody)
	}

	result = "unchanged"
	if got, err := r.Roll(context.Background(), w, "2"); err != nil || got != domain.RollResultUnchanged {
		t.Fatalf("unchanged node answer: Roll = %q, %v", got, err)
	}
	result = "rejected"
	if _, err := r.Roll(context.Background(), w, "2"); err == nil {
		t.Fatal("a rejected roll must be an error")
	}

	rollouts, err := r.Rollouts(context.Background(), w)
	if err != nil || len(rollouts) != 1 || rollouts[0].ID != "r1" || rollouts[0].Service != "web" {
		t.Fatalf("Rollouts = %+v, %v", rollouts, err)
	}
	if img, err := r.RunningImage(context.Background(), w); err != nil || img != "rg.example.com/shop/web:2" {
		t.Fatalf("RunningImage = %q, %v", img, err)
	}

	w.NodeCAFingerprint = strings.Repeat("0", 64)
	if _, err := r.Roll(context.Background(), w, "2"); err == nil {
		t.Fatal("a node whose certificate does not match the pinned fingerprint must be refused")
	}
}
