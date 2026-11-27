package sklet

import (
	"context"
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/reconcile/imageupdate/domain"
)

func TestWatchEntriesShowOnlyThisNodesRollsWithoutCredentials(t *testing.T) {
	mine := &domain.Watch{ID: "w1", StackID: "demo/hello", Name: "web", NodeSpace: "hello", Service: "web",
		ImageRepo: "traefik/whoami", CurrentTag: "v1.10.4", Status: domain.StatusIdle,
		NodeToken: "secret-token", RegistrySecret: "secret-pw"}
	core := &domain.Watch{ID: "w2", StackID: "stack-uuid", Name: "api"}
	now := time.Now()
	rolls := func(_ context.Context, id string, _ int) ([]*domain.Roll, error) {
		var out []*domain.Roll
		for i := 0; i < 8; i++ {
			out = append(out, &domain.Roll{WatchID: id, FromTag: "a", ToTag: "b", Result: domain.RollResultLive, CreatedAt: now})
		}
		return append([]*domain.Roll{{Result: domain.RollResultUnchanged}}, out...), nil
	}

	got, err := watchEntries(context.Background(), "demo", []*domain.Watch{mine, core}, rolls)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Repo != "hello" || got[0].Space != "hello" || got[0].CurrentTag != "v1.10.4" {
		t.Fatalf("entries = %+v", got)
	}
	if len(got[0].Rolls) != rollsShown || got[0].Rolls[0].Result != domain.RollResultLive {
		t.Fatalf("rolls = %+v", got[0].Rolls)
	}
}
