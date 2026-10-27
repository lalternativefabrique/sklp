package store

import (
	"context"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/lalternativefabrique/sklp/reconcile/imageupdate/domain"
)

// reverseCipher stands in for secretbox: it proves a value went through the
// sealing path (the stored bytes are not the plaintext) without a key to
// manage. A real cipher is tested where it lives.
type reverseCipher struct{}

func (reverseCipher) Encrypt(plaintext string) ([]byte, error) {
	b := []byte(plaintext)
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return b, nil
}

func (reverseCipher) Decrypt(blob []byte) (string, error) {
	s, _ := reverseCipher{}.Encrypt(string(blob))
	return string(s), nil
}

func testKVRepo(t *testing.T) *KVRepository {
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
	ctx := context.Background()
	watches, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: WatchesBucket})
	if err != nil {
		t.Fatalf("watches bucket: %v", err)
	}
	rolls, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: RollsBucket})
	if err != nil {
		t.Fatalf("rolls bucket: %v", err)
	}
	return NewKVRepository(watches, rolls, reverseCipher{})
}

func kvWatch(t *testing.T, stack, name string) *domain.Watch {
	t.Helper()
	w, err := domain.NewWatch(stack, name, "rg.example/core", "", "core",
		"https://node:6464", "production", "node-token", "sha256:fp")
	if err != nil {
		t.Fatalf("NewWatch: %v", err)
	}
	return w
}

// The node token is a credential. It must not sit in the bucket as plaintext,
// and it must come back intact — a store that mangles it silently loses the
// only way to reach the node.
func TestKVWatchSealsTheNodeToken(t *testing.T) {
	ctx := context.Background()
	r := testKVRepo(t)

	w := kvWatch(t, "s1", "shop-core")
	w.RegistrySecret = "registry-password"
	if err := r.Save(ctx, w); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := r.FindByID(ctx, w.ID)
	if err != nil || got == nil {
		t.Fatalf("FindByID: %+v, %v", got, err)
	}
	if got.NodeToken != "node-token" || got.RegistrySecret != "registry-password" {
		t.Fatalf("token=%q secret=%q, want both back in the clear", got.NodeToken, got.RegistrySecret)
	}

	// Read the raw entry: the plaintext must not be in it.
	entry, err := r.watches.Get(ctx, watchKey("s1", w.ID))
	if err != nil {
		t.Fatalf("raw get: %v", err)
	}
	if contains(entry.Value(), "node-token") || contains(entry.Value(), "registry-password") {
		t.Error("a credential is stored in the clear")
	}
}

func contains(haystack []byte, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		string(haystack) != "" && indexOf(string(haystack), needle) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// A paused watch must not be polled: FindDue is what the ticker reads.
func TestKVFindDueSkipsDisabled(t *testing.T) {
	ctx := context.Background()
	r := testKVRepo(t)

	on := kvWatch(t, "s1", "enabled")
	off := kvWatch(t, "s1", "disabled")
	off.Enabled = false
	for _, w := range []*domain.Watch{on, off} {
		if err := r.Save(ctx, w); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}

	due, err := r.FindDue(ctx)
	if err != nil {
		t.Fatalf("FindDue: %v", err)
	}
	if len(due) != 1 || due[0].Name != "enabled" {
		t.Fatalf("got %d due watches, want only the enabled one", len(due))
	}
}

// The claim is what stops two callers dispatching the same roll. Postgres uses
// a row lock; here the revision does it, and the second caller must be told no
// rather than double-dispatching.
func TestKVClaimRollIsExclusive(t *testing.T) {
	ctx := context.Background()
	r := testKVRepo(t)

	w := kvWatch(t, "s1", "shop-core")
	if err := r.Save(ctx, w); err != nil {
		t.Fatalf("Save: %v", err)
	}

	first, err := r.ClaimRoll(ctx, w.ID, "new-tag")
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	second, err := r.ClaimRoll(ctx, w.ID, "new-tag")
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if !first || second {
		t.Fatalf("first=%v second=%v, want exactly one winner", first, second)
	}
}

func TestKVClaimRollRefusesTheCurrentTag(t *testing.T) {
	ctx := context.Background()
	r := testKVRepo(t)

	w := kvWatch(t, "s1", "shop-core")
	w.CurrentTag = "20260904-0807"
	if err := r.Save(ctx, w); err != nil {
		t.Fatalf("Save: %v", err)
	}

	claimed, err := r.ClaimRoll(ctx, w.ID, "20260904-0807")
	if err != nil {
		t.Fatalf("ClaimRoll: %v", err)
	}
	if claimed {
		t.Fatal("claimed a roll onto the tag already running")
	}
}

// FinishRoll journals the roll and advances the watch, using the same
// aggregate transition the Postgres adapter calls.
func TestKVFinishRollAdvancesAndJournals(t *testing.T) {
	ctx := context.Background()
	r := testKVRepo(t)

	w := kvWatch(t, "s1", "shop-core")
	w.CurrentTag = "old"
	if err := r.Save(ctx, w); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := r.ClaimRoll(ctx, w.ID, "new"); err != nil {
		t.Fatalf("ClaimRoll: %v", err)
	}

	roll := domain.NewRoll(w.ID, "old", "new", domain.RollResultLive, "")
	if err := r.FinishRoll(ctx, w.ID, roll); err != nil {
		t.Fatalf("FinishRoll: %v", err)
	}

	got, err := r.FindByID(ctx, w.ID)
	if err != nil || got == nil {
		t.Fatalf("FindByID: %+v, %v", got, err)
	}
	if got.CurrentTag != "new" || got.Status != domain.StatusIdle {
		t.Fatalf("tag=%q status=%q, want the tag advanced and the watch idle", got.CurrentTag, got.Status)
	}
	rolls, err := r.ListRolls(ctx, w.ID, 10)
	if err != nil {
		t.Fatalf("ListRolls: %v", err)
	}
	if len(rolls) != 1 || rolls[0].ToTag != "new" {
		t.Fatalf("journal = %+v, want the roll recorded", rolls)
	}
}

// A rollback holds the tag and keeps the watch off it, rather than failing the
// watch outright — the image is bad, not the watch.
func TestKVSettleRollHoldsARolledBackTag(t *testing.T) {
	ctx := context.Background()
	r := testKVRepo(t)

	w := kvWatch(t, "s1", "shop-core")
	w.CurrentTag = "old"
	if err := r.Save(ctx, w); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := r.ClaimRoll(ctx, w.ID, "bad"); err != nil {
		t.Fatalf("ClaimRoll: %v", err)
	}
	roll := domain.NewRoll(w.ID, "old", "bad", domain.RollResultApplied, "")
	if err := r.FinishRoll(ctx, w.ID, roll); err != nil {
		t.Fatalf("FinishRoll: %v", err)
	}

	if err := r.SettleRoll(ctx, w.ID, roll, domain.RollOutcome{
		Result: domain.RollResultRolledBack, Reason: "probe failed", FinishedAt: time.Now(),
	}); err != nil {
		t.Fatalf("SettleRoll: %v", err)
	}

	got, err := r.FindByID(ctx, w.ID)
	if err != nil || got == nil {
		t.Fatalf("FindByID: %+v, %v", got, err)
	}
	if got.Status != domain.StatusHeld || got.HeldTag != "bad" || got.CurrentTag != "old" {
		t.Fatalf("status=%q held=%q tag=%q, want the bad tag held and the old one running",
			got.Status, got.HeldTag, got.CurrentTag)
	}
}

// An applied roll is owed an outcome; once settled it is not. ListPendingRolls
// is what the reattach poller reads to find rollouts it must still chase.
func TestKVListPendingRollsTracksUnsettledOnly(t *testing.T) {
	ctx := context.Background()
	r := testKVRepo(t)

	w := kvWatch(t, "s1", "shop-core")
	if err := r.Save(ctx, w); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := r.ClaimRoll(ctx, w.ID, "new"); err != nil {
		t.Fatalf("ClaimRoll: %v", err)
	}
	roll := domain.NewRoll(w.ID, "old", "new", domain.RollResultApplied, "")
	if err := r.FinishRoll(ctx, w.ID, roll); err != nil {
		t.Fatalf("FinishRoll: %v", err)
	}

	pending, err := r.ListPendingRolls(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("ListPendingRolls: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("got %d pending, want the applied roll", len(pending))
	}

	if err := r.SettleRoll(ctx, w.ID, roll, domain.RollOutcome{
		Result: domain.RollResultLive, FinishedAt: time.Now(),
	}); err != nil {
		t.Fatalf("SettleRoll: %v", err)
	}
	pending, err = r.ListPendingRolls(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("ListPendingRolls: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("got %d pending after settling, want none", len(pending))
	}
}

// A process that died mid-roll leaves a watch claimed forever, and nothing
// would ever check it again. ResetOrphans frees only those: a watch whose roll
// is still owed an outcome must be left alone.
func TestKVResetOrphansFreesOnlyStuckWatches(t *testing.T) {
	ctx := context.Background()
	r := testKVRepo(t)

	stuck := kvWatch(t, "s1", "stuck")
	inFlight := kvWatch(t, "s1", "in-flight")
	for _, w := range []*domain.Watch{stuck, inFlight} {
		if err := r.Save(ctx, w); err != nil {
			t.Fatalf("Save: %v", err)
		}
		if _, err := r.ClaimRoll(ctx, w.ID, "new"); err != nil {
			t.Fatalf("ClaimRoll: %v", err)
		}
	}
	// Only in-flight has a roll waiting to settle.
	roll := domain.NewRoll(inFlight.ID, "old", "new", domain.RollResultApplied, "")
	if err := r.FinishRoll(ctx, inFlight.ID, roll); err != nil {
		t.Fatalf("FinishRoll: %v", err)
	}

	reset, err := r.ResetOrphans(ctx, "restarted mid-roll")
	if err != nil {
		t.Fatalf("ResetOrphans: %v", err)
	}
	if reset != 1 {
		t.Fatalf("reset %d watches, want only the stuck one", reset)
	}
	got, err := r.FindByID(ctx, inFlight.ID)
	if err != nil || got == nil {
		t.Fatalf("FindByID: %+v, %v", got, err)
	}
	if got.Status != domain.StatusRolling {
		t.Errorf("the in-flight watch was reset to %q; its outcome is still owed", got.Status)
	}
}

// Release lifts a hold so the poller resumes.
func TestKVReleaseClearsHoldAndPin(t *testing.T) {
	ctx := context.Background()
	r := testKVRepo(t)

	w := kvWatch(t, "s1", "shop-core")
	w.Status = domain.StatusHeld
	w.HeldTag = "bad"
	w.PinnedTag = "bad"
	w.LastError = "probe failed"
	if err := r.Save(ctx, w); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if err := r.Release(ctx, w.ID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	got, err := r.FindByID(ctx, w.ID)
	if err != nil || got == nil {
		t.Fatalf("FindByID: %+v, %v", got, err)
	}
	if got.HeldTag != "" || got.PinnedTag != "" || got.LastError != "" || got.Status != domain.StatusIdle {
		t.Fatalf("got %+v, want the hold, the pin and the error all cleared", got)
	}
}

// One stack must not see another's watches.
func TestKVFindByStackScopes(t *testing.T) {
	ctx := context.Background()
	r := testKVRepo(t)

	if err := r.Save(ctx, kvWatch(t, "s1", "mine")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := r.Save(ctx, kvWatch(t, "s2", "theirs")); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := r.FindByStackID(ctx, "s1")
	if err != nil {
		t.Fatalf("FindByStackID: %v", err)
	}
	if len(got) != 1 || got[0].Name != "mine" {
		t.Fatalf("got %+v, want only s1's watch", got)
	}
}

// The journal returns newest first, which is what the UI lists.
func TestKVListRollsIsNewestFirst(t *testing.T) {
	ctx := context.Background()
	r := testKVRepo(t)

	w := kvWatch(t, "s1", "shop-core")
	if err := r.Save(ctx, w); err != nil {
		t.Fatalf("Save: %v", err)
	}
	for _, tag := range []string{"first", "second", "third"} {
		roll := domain.NewRoll(w.ID, "old", tag, domain.RollResultLive, "")
		if err := r.FinishRoll(ctx, w.ID, roll); err != nil {
			t.Fatalf("FinishRoll %s: %v", tag, err)
		}
		time.Sleep(time.Millisecond)
	}

	rolls, err := r.ListRolls(ctx, w.ID, 2)
	if err != nil {
		t.Fatalf("ListRolls: %v", err)
	}
	if len(rolls) != 2 {
		t.Fatalf("got %d rolls, want the limit honoured", len(rolls))
	}
	if rolls[0].ToTag != "third" || rolls[1].ToTag != "second" {
		t.Fatalf("got %s then %s, want newest first", rolls[0].ToTag, rolls[1].ToTag)
	}
}
