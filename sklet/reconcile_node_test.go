package sklet

import (
	"context"
	"errors"
	"testing"

	"github.com/lalternativefabrique/sklp/reconcile"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
)

func TestLoopbackControlURL(t *testing.T) {
	cases := map[string]string{
		":6464":            "https://127.0.0.1:6464",
		"0.0.0.0:6464":     "https://127.0.0.1:6464",
		"10.200.0.12:6464": "https://10.200.0.12:6464",
		"garbage":          "https://127.0.0.1:6464",
	}
	for listen, want := range cases {
		if got := loopbackControlURL(listen); got != want {
			t.Errorf("loopbackControlURL(%q) = %q, want %q", listen, got, want)
		}
	}
}

func TestNodeReconcileStartsOnItsFirstFollow(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	n := startNodeReconcile(ctx, ":6464")
	if stores, _ := n.running(); stores != nil {
		t.Fatal("a node that never reconciled must start nothing")
	}
	if list, err := n.ListFollows(ctx); err != nil || len(list.([]reconcile.Follow)) != 0 {
		t.Fatalf("idle list = %v, %v", list, err)
	}
	if _, err := n.AddFollow(ctx, control.FollowRequest{Repo: "https://tok@github.com/moi/hello"}); !errors.Is(err, control.ErrFollowInvalid) {
		t.Fatalf("a credential in the URL must be refused before anything starts, got %v", err)
	}
	if stores, _ := n.running(); stores != nil {
		t.Fatal("a refused follow must not start the reconcile")
	}

	if _, err := n.AddFollow(ctx, control.FollowRequest{Repo: "https://github.com/moi/hello"}); err != nil {
		t.Fatal(err)
	}
	if !reconcileConfigured() {
		t.Fatal("after a follow the node's reconcile state exists, so the next boot resumes it")
	}
	list, _ := n.ListFollows(ctx)
	if got := list.([]reconcile.Follow); len(got) != 1 || got[0].Name != "hello" {
		t.Fatalf("list = %+v", got)
	}
	if _, err := n.AddFollow(ctx, control.FollowRequest{Repo: "https://github.com/other/hello"}); !errors.Is(err, control.ErrFollowConflict) {
		t.Fatalf("same name: %v", err)
	}
	if err := n.RemoveFollow(ctx, "hello"); err != nil {
		t.Fatal(err)
	}
	if err := n.RemoveFollow(ctx, "hello"); !errors.Is(err, control.ErrFollowMissing) {
		t.Fatalf("removed twice: %v", err)
	}
	n.Close()
	again, err := reconcile.OpenEmbeddedJetStream(reconcile.CacheRoot())
	if err != nil {
		t.Fatalf("Close must release the state: %v", err)
	}
	again.Close()
}
