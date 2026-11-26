package reconcile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/reconcile/deployconfig/nodeapi"
	"github.com/lalternativefabrique/sklp/reconcile/imageupdate/domain"
)

func TestARepoRollsOnlyTheSpacesItOwns(t *testing.T) {
	owner := ""
	rolled := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/manifest"):
			_ = json.NewEncoder(w).Encode(map[string]string{"stack": owner})
		case strings.HasSuffix(r.URL.Path, "/image"):
			rolled++
			_ = json.NewEncoder(w).Encode(map[string]string{"result": "configured"})
		}
	}))
	defer srv.Close()
	sum := sha256.Sum256(srv.Certificate().Raw)
	node := &LocalNode{Name: "n1", ControlURL: srv.URL, Token: "t", CAFingerprint: hex.EncodeToString(sum[:])}
	roller := localCredentials{inner: nodeapi.NewControlRoller(), local: node}
	watch := &domain.Watch{Name: "blog-app", StackID: "n1/blog", Service: "app", NodeSpace: "blog", ImageRepo: "ghcr.io/moi/blog"}

	owner = "blog/web"
	if _, err := roller.Roll(context.Background(), watch, "2"); err != nil || rolled != 1 {
		t.Fatalf("own space: err=%v rolled=%d", err, rolled)
	}
	for _, foreign := range []string{"other/web", "web", ""} {
		owner = foreign
		if _, err := roller.Roll(context.Background(), watch, "2"); err == nil {
			t.Errorf("space owned by %q must not be rolled by repo blog", foreign)
		}
	}
	if rolled != 1 {
		t.Fatalf("a refused roll reached the node: %d rolls", rolled)
	}

	shared := *watch
	shared.StackID = "n1"
	owner = "web"
	if _, err := roller.Roll(context.Background(), &shared, "2"); err != nil {
		t.Fatalf("a watch reconcile.yaml declares under the node scope rolls as before: %v", err)
	}
}
