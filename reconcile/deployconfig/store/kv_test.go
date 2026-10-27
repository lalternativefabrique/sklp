package store

import (
	"context"
	"slices"
	"sort"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/lalternativefabrique/sklp/reconcile/deployconfig/domain"
)

// testKV starts an embedded NATS with JetStream on a temp store and returns a
// bucket bound to it. A real server rather than a fake: the behaviour worth
// testing here — key filters, and a Get on a missing key — belongs to
// JetStream, and a hand-written double would only assert what I assumed.
func testKV(t *testing.T) jetstream.KeyValue {
	t.Helper()
	opts := &natsserver.Options{
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
		NoLog:     true,
		NoSigs:    true,
	}
	srv, err := natsserver.NewServer(opts)
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
	kv, err := js.CreateKeyValue(context.Background(), jetstream.KeyValueConfig{Bucket: DeploymentsBucket})
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	return kv
}

func deployment(t *testing.T, stack, space string) *domain.Deployment {
	t.Helper()
	d, err := domain.NewDeployment(stack, ".sklp/prod/deploy.yaml", "node-a", "", space)
	if err != nil {
		t.Fatalf("NewDeployment: %v", err)
	}
	return d
}

func TestKVRepositoryRoundTripsADeployment(t *testing.T) {
	ctx := context.Background()
	r := NewKVRepository(testKV(t))

	d := deployment(t, "s1", "shop-production")
	d.RecordApplied("hash-1", time.Now())
	if err := r.Save(ctx, d); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := r.FindBySpace(ctx, "s1", "shop-production")
	if err != nil {
		t.Fatalf("FindBySpace: %v", err)
	}
	if got == nil {
		t.Fatal("FindBySpace returned nothing for a saved deployment")
	}
	if got.ID != d.ID || got.Node != "node-a" || got.LastAppliedHash != "hash-1" {
		t.Fatalf("got %+v, want the saved deployment", got)
	}
	if got.LastAppliedAt == nil {
		t.Error("LastAppliedAt did not survive the round trip")
	}
}

// A space with no deployment reads as nil, not as an error: the reconcile
// handler treats nil as "first deploy" and creates the row, so returning an
// error here would turn every first deploy into a failure.
func TestKVRepositoryReportsAMissingSpaceAsNil(t *testing.T) {
	r := NewKVRepository(testKV(t))

	got, err := r.FindBySpace(context.Background(), "s1", "never-deployed")
	if err != nil {
		t.Fatalf("FindBySpace: %v", err)
	}
	if got != nil {
		t.Fatalf("got %+v, want nil for a space with no deployment", got)
	}
}

// The key is (stack, space), which is what lets several repos of one stack
// declare the same manifest path without sharing a row — the collision
// migration 000094 fixed on the SQL side holds here by construction.
func TestKVRepositoryKeepsOneDeploymentPerSpace(t *testing.T) {
	ctx := context.Background()
	r := NewKVRepository(testKV(t))

	for _, space := range []string{"shop-production", "acme-production", "demo-production"} {
		if err := r.Save(ctx, deployment(t, "s1", space)); err != nil {
			t.Fatalf("Save %s: %v", space, err)
		}
	}

	got, err := r.FindByStackID(ctx, "s1")
	if err != nil {
		t.Fatalf("FindByStackID: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d deployments, want 3 — one per space despite one shared path", len(got))
	}
}

// Another stack's spaces must not leak into a stack's listing.
func TestKVRepositoryScopesListingToItsStack(t *testing.T) {
	ctx := context.Background()
	r := NewKVRepository(testKV(t))

	if err := r.Save(ctx, deployment(t, "s1", "mine")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := r.Save(ctx, deployment(t, "s2", "theirs")); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := r.FindByStackID(ctx, "s1")
	if err != nil {
		t.Fatalf("FindByStackID: %v", err)
	}
	if len(got) != 1 || got[0].Space != "mine" {
		t.Fatalf("got %+v, want only s1's deployment", got)
	}
}

// An empty bucket is not an error: a node reconciling for the first time has
// no deployments, and the sweep that reads them must not fail on that.
func TestKVRepositoryReadsAnEmptyBucket(t *testing.T) {
	ctx := context.Background()
	r := NewKVRepository(testKV(t))

	got, err := r.FindByStackID(ctx, "s1")
	if err != nil {
		t.Fatalf("FindByStackID on an empty bucket: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d, want none", len(got))
	}
}

// FindApplied selects what the health check must re-poll: everything that was
// applied, ready ones included, since a converged space can fall over later
// and only a re-check would notice. A never-applied row has nothing to check.
func TestKVRepositoryFindsEveryAppliedDeployment(t *testing.T) {
	ctx := context.Background()
	r := NewKVRepository(testKV(t))
	now := time.Now()

	converging := deployment(t, "s1", "converging")
	converging.RecordApplied("h", now)

	ready := deployment(t, "s1", "ready")
	ready.RecordApplied("h", now)
	ready.RecordHealth(true, "", now)

	failed := deployment(t, "s1", "failed")
	failed.RecordApplied("h", now)
	failed.RecordHealth(false, "boom", now)

	neverApplied := deployment(t, "s1", "never-applied")

	for _, d := range []*domain.Deployment{converging, ready, failed, neverApplied} {
		if err := r.Save(ctx, d); err != nil {
			t.Fatalf("Save %s: %v", d.Space, err)
		}
	}

	got, err := r.FindApplied(ctx)
	if err != nil {
		t.Fatalf("FindApplied: %v", err)
	}
	spaces := make([]string, len(got))
	for i, d := range got {
		spaces[i] = d.Space
	}
	sort.Strings(spaces)
	want := []string{"converging", "failed", "ready"}
	if !slices.Equal(spaces, want) {
		t.Fatalf("got %v, want %v", spaces, want)
	}
}

// Update replaces in place rather than accumulating: the key is the identity,
// so a re-apply must not leave the previous state readable.
func TestKVRepositoryUpdateReplacesInPlace(t *testing.T) {
	ctx := context.Background()
	r := NewKVRepository(testKV(t))

	d := deployment(t, "s1", "production")
	d.RecordApplied("first", time.Now())
	if err := r.Save(ctx, d); err != nil {
		t.Fatalf("Save: %v", err)
	}

	d.RecordApplied("second", time.Now())
	if err := r.Update(ctx, d); err != nil {
		t.Fatalf("Update: %v", err)
	}

	all, err := r.FindByStackID(ctx, "s1")
	if err != nil {
		t.Fatalf("FindByStackID: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("got %d rows after an update, want 1", len(all))
	}
	if all[0].LastAppliedHash != "second" {
		t.Fatalf("hash = %q, want the updated one", all[0].LastAppliedHash)
	}
}
