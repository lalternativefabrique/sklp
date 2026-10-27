package store

import (
	"bytes"
	"context"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/lalternativefabrique/sklp/reconcile/gateway/domain"
)

// testJS starts an embedded NATS with JetStream and returns the named buckets.
func testJS(t *testing.T, buckets ...string) []jetstream.KeyValue {
	t.Helper()
	srv, err := natsserver.NewServer(&natsserver.Options{
		Port: -1, JetStream: true, StoreDir: t.TempDir(), NoLog: true, NoSigs: true,
	})
	if err != nil {
		t.Fatalf("nats server: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats server did not start")
	}
	t.Cleanup(srv.Shutdown)

	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)

	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	out := make([]jetstream.KeyValue, 0, len(buckets))
	for _, name := range buckets {
		kv, err := js.CreateKeyValue(context.Background(), jetstream.KeyValueConfig{Bucket: name})
		if err != nil {
			t.Fatalf("create bucket %s: %v", name, err)
		}
		out = append(out, kv)
	}
	return out
}

func testNodeRepo(t *testing.T) *KVNodeRepository {
	t.Helper()
	kvs := testJS(t, NodesBucket, NodeNamesBucket)
	return NewKVNodeRepository(kvs[0], kvs[1])
}

func newNode(t *testing.T, stack, name string) *domain.Node {
	t.Helper()
	n, err := domain.NewNode(stack, name, "https://203.0.113.10:6464", []byte("sealed-token"), "sha256:deadbeef")
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	return n
}

// The sealed token is the one thing a node record holds that exists nowhere
// else, so a round trip that silently mangled those bytes would lose the only
// way to reach the node. JSON base64-encodes []byte; this pins it.
func TestKVNodeRepositoryPreservesTheSealedToken(t *testing.T) {
	ctx := context.Background()
	r := testNodeRepo(t)

	n := newNode(t, "s1", "node-a")
	if err := r.Save(ctx, n); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := r.FindByID(ctx, "s1", n.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if got == nil {
		t.Fatal("FindByID returned nothing for a saved node")
	}
	if !bytes.Equal(got.EncryptedToken, []byte("sealed-token")) {
		t.Fatalf("token = %q, want the bytes that went in", got.EncryptedToken)
	}
	if got.CAFingerprint != "sha256:deadbeef" || got.ServerURL != "https://203.0.113.10:6464" {
		t.Fatalf("got %+v, want the saved transport", got)
	}
}

// A deploy-root names a node by name, so this lookup runs on every reconcile.
func TestKVNodeRepositoryResolvesByName(t *testing.T) {
	ctx := context.Background()
	r := testNodeRepo(t)

	n := newNode(t, "s1", "node-a")
	if err := r.Save(ctx, n); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := r.FindByName(ctx, "s1", "node-a")
	if err != nil {
		t.Fatalf("FindByName: %v", err)
	}
	if got == nil || got.ID != n.ID {
		t.Fatalf("got %+v, want the node saved under that name", got)
	}
}

// A rename must not leave the old name resolving: a deploy-root still naming
// it would otherwise keep deploying to a node the operator renamed away.
func TestKVNodeRepositoryDropsTheOldNameOnRename(t *testing.T) {
	ctx := context.Background()
	r := testNodeRepo(t)

	n := newNode(t, "s1", "old-name")
	if err := r.Save(ctx, n); err != nil {
		t.Fatalf("Save: %v", err)
	}
	n.Name = "new-name"
	if err := r.Update(ctx, n); err != nil {
		t.Fatalf("Update: %v", err)
	}

	stale, err := r.FindByName(ctx, "s1", "old-name")
	if err != nil {
		t.Fatalf("FindByName(old): %v", err)
	}
	if stale != nil {
		t.Errorf("the old name still resolves to %+v", stale)
	}
	fresh, err := r.FindByName(ctx, "s1", "new-name")
	if err != nil || fresh == nil {
		t.Fatalf("the new name does not resolve: %+v, %v", fresh, err)
	}
}

// Deleting a node must take its name with it, or the index outlives the record
// and FindByName resolves an id that no longer exists.
func TestKVNodeRepositoryDeleteClearsTheNameIndex(t *testing.T) {
	ctx := context.Background()
	r := testNodeRepo(t)

	n := newNode(t, "s1", "node-a")
	if err := r.Save(ctx, n); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := r.Delete(ctx, "s1", n.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	byName, err := r.FindByName(ctx, "s1", "node-a")
	if err != nil {
		t.Fatalf("FindByName after delete: %v", err)
	}
	if byName != nil {
		t.Errorf("name still resolves to %+v after the node was deleted", byName)
	}
	byID, err := r.FindByID(ctx, "s1", n.ID)
	if err != nil || byID != nil {
		t.Errorf("node still readable by id after delete: %+v, %v", byID, err)
	}
}

// One stack must not see another's nodes, and the same name in two stacks must
// resolve to two different nodes.
func TestKVNodeRepositoryScopesByStack(t *testing.T) {
	ctx := context.Background()
	r := testNodeRepo(t)

	mine := newNode(t, "s1", "shared-name")
	theirs := newNode(t, "s2", "shared-name")
	for _, n := range []*domain.Node{mine, theirs} {
		if err := r.Save(ctx, n); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}

	listed, err := r.ListByStack(ctx, "s1")
	if err != nil {
		t.Fatalf("ListByStack: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != mine.ID {
		t.Fatalf("got %d nodes for s1, want only its own", len(listed))
	}
	got, err := r.FindByName(ctx, "s2", "shared-name")
	if err != nil || got == nil || got.ID != theirs.ID {
		t.Fatalf("the same name in another stack resolved to %+v", got)
	}
}

func testRouteRepo(t *testing.T) *KVRouteRepository {
	t.Helper()
	return NewKVRouteRepository(testJS(t, RoutesBucket)[0])
}

func newRoute(t *testing.T, nodeID, host, path string) *domain.Route {
	t.Helper()
	route, err := domain.NewRoute(nodeID, "production", "api", host, path, 0, 0)
	if err != nil {
		t.Fatalf("NewRoute: %v", err)
	}
	return route
}

// FindByHostPath guards uniqueness when a route is created: two routes on one
// host and path would make which service answers a matter of ordering.
func TestKVRouteRepositoryFindsAClashByHostAndPath(t *testing.T) {
	ctx := context.Background()
	r := testRouteRepo(t)

	route := newRoute(t, "n1", "shop.fr", "/")
	if err := r.Save(ctx, route); err != nil {
		t.Fatalf("Save: %v", err)
	}

	clash, err := r.FindByHostPath(ctx, "n1", "shop.fr", "/")
	if err != nil {
		t.Fatalf("FindByHostPath: %v", err)
	}
	if clash == nil || clash.ID != route.ID {
		t.Fatalf("got %+v, want the existing route", clash)
	}

	free, err := r.FindByHostPath(ctx, "n1", "shop.fr", "/other")
	if err != nil {
		t.Fatalf("FindByHostPath: %v", err)
	}
	if free != nil {
		t.Errorf("an unused path reported a clash: %+v", free)
	}
}

// Routes are keyed by node, so one node's routes must not leak into another's.
func TestKVRouteRepositoryScopesByNode(t *testing.T) {
	ctx := context.Background()
	r := testRouteRepo(t)

	if err := r.Save(ctx, newRoute(t, "n1", "a.fr", "/")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := r.Save(ctx, newRoute(t, "n2", "b.fr", "/")); err != nil {
		t.Fatalf("Save: %v", err)
	}

	listed, err := r.ListByNode(ctx, "n1")
	if err != nil {
		t.Fatalf("ListByNode: %v", err)
	}
	if len(listed) != 1 || listed[0].Host != "a.fr" {
		t.Fatalf("got %+v, want only n1's route", listed)
	}
}

func TestKVRouteRepositoryDeleteRemovesTheRoute(t *testing.T) {
	ctx := context.Background()
	r := testRouteRepo(t)

	route := newRoute(t, "n1", "shop.fr", "/")
	if err := r.Save(ctx, route); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := r.Delete(ctx, "n1", route.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	got, err := r.FindByID(ctx, "n1", route.ID)
	if err != nil || got != nil {
		t.Fatalf("route still readable after delete: %+v, %v", got, err)
	}
}

// Deleting what is not there is not an error: a reconcile that removes a route
// twice (a retry, a race with another writer) must converge, not fail.
func TestKVRouteRepositoryDeleteIsIdempotent(t *testing.T) {
	r := testRouteRepo(t)
	if err := r.Delete(context.Background(), "n1", "never-existed"); err != nil {
		t.Fatalf("deleting a missing route: %v", err)
	}
}
