package reconcile

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	imagedomain "github.com/lalternativefabrique/sklp/reconcile/imageupdate/domain"
)

func openTestStores(t *testing.T) (*Stores, string) {
	t.Helper()
	cache := t.TempDir()
	e, err := OpenEmbeddedJetStream(cache)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	stores, err := OpenStores(context.Background(), e.JS, nil)
	if err != nil {
		t.Fatal(err)
	}
	return stores, cache
}

func TestNormalizeFollowRefusesWhatItCannotServeBack(t *testing.T) {
	cases := map[string]bool{
		"https://github.com/moi/hello":         true,
		"/home/architect/demo-repos/hello.git": true,
		"https://tok@github.com/moi/hello":     false,
		"https://user:pw@github.com/moi/hello": false,
		"git@github.com:moi/hello.git":         false,
		"http://github.com/moi/hello":          false,
		"https://github.com":                   false,
	}
	for repo, ok := range cases {
		_, err := NormalizeFollow(Follow{Repo: repo})
		if (err == nil) != ok {
			t.Errorf("NormalizeFollow(%q) err=%v, want accepted=%v", repo, err, ok)
		}
	}
	f, err := NormalizeFollow(Follow{Repo: "https://github.com/Moi/Hello"})
	if err != nil || f.Name != "hello" || f.Branch != "main" {
		t.Fatalf("defaults: %+v, %v", f, err)
	}
}

func TestFollowStoreAddListRemove(t *testing.T) {
	stores, _ := openTestStores(t)
	ctx := context.Background()
	if _, err := stores.Follows.Add(ctx, Follow{Repo: "https://github.com/moi/hello"}); err != nil {
		t.Fatal(err)
	}
	if _, err := stores.Follows.Add(ctx, Follow{Repo: "https://github.com/other/hello"}); !errors.Is(err, ErrFollowTaken) {
		t.Fatalf("a second repo under the same name must be refused, got %v", err)
	}
	list, err := stores.Follows.List(ctx)
	if err != nil || len(list) != 1 || list[0].Repo != "https://github.com/moi/hello" || list[0].AddedAt.IsZero() {
		t.Fatalf("List = %+v, %v", list, err)
	}
	if err := stores.Follows.Remove(ctx, "hello"); err != nil {
		t.Fatal(err)
	}
	if err := stores.Follows.Remove(ctx, "hello"); !errors.Is(err, ErrFollowNotFound) {
		t.Fatalf("removing twice: %v", err)
	}
}

func TestRunnerServesDeclaredAndFollowedRepos(t *testing.T) {
	stores, cache := openTestStores(t)
	ctx := context.Background()
	node := &LocalNode{Name: "n1", ControlURL: "https://127.0.0.1:6464", Token: "t"}
	declared := []TargetConfig{{Repo: "/srv/infra.git", Name: "infra"}}
	r := NewRunner(stores, node, declared, cache, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if got := names(r.Targets(ctx)); got != "infra" {
		t.Fatalf("declared only: %q", got)
	}
	_, _ = stores.Follows.Add(ctx, Follow{Repo: "https://github.com/moi/hello"})
	_, _ = stores.Follows.Add(ctx, Follow{Repo: "https://github.com/moi/infra", Name: "infra"})
	targets := r.Targets(ctx)
	if got := names(targets); got != "infra,hello" {
		t.Fatalf("declared wins a name clash, followed is added: %q", got)
	}
	for _, tg := range targets {
		if len(tg.Reconcilers) != 2 || tg.Branch != "main" {
			t.Fatalf("target %q: %d reconcilers, branch %q", tg.Repo, len(tg.Reconcilers), tg.Branch)
		}
	}
	if again := r.Targets(ctx); again[1].Source != targets[1].Source {
		t.Fatal("a repo still followed must keep its mirror across passes")
	}
	_ = stores.Follows.Remove(ctx, "hello")
	if got := names(r.Targets(ctx)); got != "infra" {
		t.Fatalf("unfollowed: %q", got)
	}
}

func TestReconcileStateOpensOnce(t *testing.T) {
	cache := t.TempDir()
	e, err := OpenEmbeddedJetStream(cache)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenEmbeddedJetStream(cache); !errors.Is(err, ErrStateBusy) {
		t.Fatalf("a second opener must be refused, got %v", err)
	}
	e.Close()
	again, err := OpenEmbeddedJetStream(cache)
	if err != nil {
		t.Fatalf("once closed, the state opens again: %v", err)
	}
	again.Close()
}

func names(ts []Target) string {
	out := ""
	for i, t := range ts {
		if i > 0 {
			out += ","
		}
		out += t.Repo
	}
	return out
}

func TestNormalizeFollowNeverEchoesACredential(t *testing.T) {
	for _, repo := range []string{"https://ghp_SECRET@github.com/moi/hello", "https://x:ghp_SECRET@github.com/moi/hello", "https://ghp_SECRET@github.com"} {
		_, err := NormalizeFollow(Follow{Repo: repo})
		if err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Errorf("NormalizeFollow(%q) err = %v: must refuse without echoing the credential", repo, err)
		}
	}
}

func TestRunnerGivesEachRepoItsOwnScopeAndDropsTheSharedOne(t *testing.T) {
	stores, cache := openTestStores(t)
	ctx := context.Background()
	node := &LocalNode{Name: "n1", ControlURL: "https://127.0.0.1:6464", Token: "t"}

	legacy := &imagedomain.Watch{ID: "w-legacy", StackID: "n1", Name: "old", Service: "web", ImageRepo: "docker.io/x/y", TagPattern: imagedomain.DefaultTagPattern}
	if err := stores.Watches.Save(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	_, _ = stores.Follows.Add(ctx, Follow{Repo: "https://github.com/moi/blog"})
	r := NewRunner(stores, node, nil, cache, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.Targets(ctx)

	if left, _ := stores.Watches.FindByStackID(ctx, "n1"); len(left) != 0 {
		t.Fatalf("a watch under the shared scope would keep rolling: %+v", left)
	}
	if got := string(RepoScope(node, "blog")); got != "n1/blog" {
		t.Fatalf("RepoScope = %q", got)
	}
}
