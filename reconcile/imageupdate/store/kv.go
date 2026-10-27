package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/lalternativefabrique/sklp/reconcile/imageupdate/domain"
)

// kv_repository.go is the JetStream KV adapter behind domain.Repository, for
// the reconciler running on the node (ADR-014).
//
// The roll transitions it needs live on the Watch aggregate (ClaimFor, Finish,
// Settle), so this adapter decides nothing about them — it loads, calls, and
// persists, exactly as the Postgres one does. That split is what lets the two
// stores stay in agreement.

const (
	// WatchesBucket holds one entry per watch, keyed "<stackID>.<watchID>".
	WatchesBucket = "WATCHES"
	// RollsBucket holds the roll journal, keyed
	// "<watchID>.<unix-nanos>.<rollID>". The timestamp sits in the key so the
	// journal sorts by time without reading a single value, and a roll stays
	// addressable for the update SettleRoll performs — which is why rolls are
	// a KV here and not a stream: a stream is append-only, and a roll is
	// written once and completed later.
	RollsBucket = "ROLLS"
)

// KVRepository implements domain.Repository over JetStream KV.
type KVRepository struct {
	watches jetstream.KeyValue
	rolls   jetstream.KeyValue
	cipher  Sealer
}

// Sealer encrypts the node token and registry secret a watch carries. It is
// the shape pkg/secretbox.Cipher already has, so the same cipher serves both
// adapters — a watch is encrypted at rest in the KV as it is in Postgres, not
// only in the store that happens to be a database.
type Sealer interface {
	Encrypt(plaintext string) ([]byte, error)
	Decrypt(blob []byte) (string, error)
}

func NewKVRepository(watches, rolls jetstream.KeyValue, cipher Sealer) *KVRepository {
	return &KVRepository{watches: watches, rolls: rolls, cipher: cipher}
}

// storedWatch is the wire shape. The domain carries the node token and the
// registry secret as plaintext strings; both are sealed before they reach the
// bucket, so the KV never holds a credential in the clear.
type storedWatch struct {
	Watch          domain.Watch
	SealedToken    []byte
	SealedRegistry []byte
}

func (r *KVRepository) Save(ctx context.Context, w *domain.Watch) error {
	return r.put(ctx, w)
}

func (r *KVRepository) Update(ctx context.Context, w *domain.Watch) error {
	return r.put(ctx, w)
}

func (r *KVRepository) put(ctx context.Context, w *domain.Watch) error {
	body, err := r.encode(w)
	if err != nil {
		return err
	}
	if _, err := r.watches.Put(ctx, watchKey(w.StackID, w.ID), body); err != nil {
		return fmt.Errorf("put watch %s: %w", w.ID, err)
	}
	return nil
}

func (r *KVRepository) encode(w *domain.Watch) ([]byte, error) {
	stored := storedWatch{Watch: *w}
	sealedToken, err := r.seal(w.NodeToken)
	if err != nil {
		return nil, fmt.Errorf("seal node token: %w", err)
	}
	sealedRegistry, err := r.seal(w.RegistrySecret)
	if err != nil {
		return nil, fmt.Errorf("seal registry secret: %w", err)
	}
	stored.SealedToken = sealedToken
	stored.SealedRegistry = sealedRegistry
	stored.Watch.NodeToken = ""
	stored.Watch.RegistrySecret = ""

	body, err := json.Marshal(stored)
	if err != nil {
		return nil, fmt.Errorf("marshal watch: %w", err)
	}
	return body, nil
}

func (r *KVRepository) seal(plaintext string) ([]byte, error) {
	if plaintext == "" || r.cipher == nil {
		return nil, nil
	}
	return r.cipher.Encrypt(plaintext)
}

func (r *KVRepository) decode(raw []byte) (*domain.Watch, error) {
	var stored storedWatch
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, fmt.Errorf("decode watch: %w", err)
	}
	w := stored.Watch
	token, err := r.open(stored.SealedToken)
	if err != nil {
		return nil, fmt.Errorf("open node token: %w", err)
	}
	secret, err := r.open(stored.SealedRegistry)
	if err != nil {
		return nil, fmt.Errorf("open registry secret: %w", err)
	}
	w.NodeToken = token
	w.RegistrySecret = secret
	return &w, nil
}

func (r *KVRepository) open(sealed []byte) (string, error) {
	if len(sealed) == 0 || r.cipher == nil {
		return "", nil
	}
	return r.cipher.Decrypt(sealed)
}

// FindByID scans, because the key is (stack, watch) and the port takes an id
// alone. A node holds a handful of watches, so the scan costs less than a
// second index that must survive every write.
func (r *KVRepository) FindByID(ctx context.Context, id string) (*domain.Watch, error) {
	watches, err := r.all(ctx, ">")
	if err != nil {
		return nil, err
	}
	for _, w := range watches {
		if w.ID == id {
			return w, nil
		}
	}
	return nil, nil
}

func (r *KVRepository) FindByStackID(ctx context.Context, stackID string) ([]*domain.Watch, error) {
	return r.all(ctx, stackID+".*")
}

// FindDue returns every enabled watch, matching the Postgres adapter: the
// poller decides on its own cadence which of them to check.
func (r *KVRepository) FindDue(ctx context.Context) ([]*domain.Watch, error) {
	watches, err := r.all(ctx, ">")
	if err != nil {
		return nil, err
	}
	out := watches[:0]
	for _, w := range watches {
		if w.Enabled {
			out = append(out, w)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (r *KVRepository) Delete(ctx context.Context, id string) error {
	keys, err := listKeys(ctx, r.watches, ">")
	if err != nil {
		return err
	}
	for _, key := range keys {
		entry, err := r.watches.Get(ctx, key)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("get %s: %w", key, err)
		}
		w, err := r.decode(entry.Value())
		if err != nil {
			return err
		}
		if w.ID == id {
			if derr := r.watches.Delete(ctx, key); derr != nil && !errors.Is(derr, jetstream.ErrKeyNotFound) {
				return fmt.Errorf("delete watch %s: %w", id, derr)
			}
			return nil
		}
	}
	return nil
}

// ClaimRoll takes the watch for a roll, refusing when another caller already
// holds it.
//
// Postgres serialises this with SELECT ... FOR UPDATE; a KV has no lock, so
// the entry's revision does the same job optimistically: Update fails if
// anything wrote between the read and the write, and a lost race reports "not
// claimed" rather than double-dispatching. On a node there is one process and
// no contention, so this is correctness insurance rather than a hot path.
func (r *KVRepository) ClaimRoll(ctx context.Context, watchID, toTag string) (bool, error) {
	key, entry, w, err := r.findEntry(ctx, watchID)
	if err != nil {
		return false, err
	}
	if w == nil {
		return false, domain.ErrNotFound
	}
	if !w.ClaimFor(toTag, time.Now()) {
		return false, nil
	}
	body, err := r.encode(w)
	if err != nil {
		return false, err
	}
	if _, err := r.watches.Update(ctx, key, body, entry.Revision()); err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) || strings.Contains(err.Error(), "wrong last sequence") {
			// Someone else claimed it between the read and the write.
			return false, nil
		}
		return false, fmt.Errorf("claim watch %s: %w", watchID, err)
	}
	return true, nil
}

// FinishRoll records the node's immediate answer. The roll is journalled
// BEFORE the watch is written: a crash between the two then leaves a roll with
// no state change, which the next reconcile corrects, rather than a watch
// advanced with no trace of why.
func (r *KVRepository) FinishRoll(ctx context.Context, watchID string, roll *domain.Roll) error {
	key, _, w, err := r.findEntry(ctx, watchID)
	if err != nil {
		return err
	}
	if w == nil {
		return domain.ErrNotFound
	}
	if err := r.putRoll(ctx, roll); err != nil {
		return err
	}
	w.Finish(roll, time.Now())
	body, err := r.encode(w)
	if err != nil {
		return err
	}
	if _, err := r.watches.Put(ctx, key, body); err != nil {
		return fmt.Errorf("finish roll on %s: %w", watchID, err)
	}
	return nil
}

// SettleRoll records the rollout outcome the node reported later.
func (r *KVRepository) SettleRoll(ctx context.Context, watchID string, roll *domain.Roll, outcome domain.RollOutcome) error {
	key, _, w, err := r.findEntry(ctx, watchID)
	if err != nil {
		return err
	}
	if w == nil {
		return domain.ErrNotFound
	}
	settled := *roll
	settled.Result = outcome.Result
	settled.ErrorMessage = outcome.Reason
	settled.ExitCode = outcome.ExitCode
	settled.LogTail = outcome.LogTail
	finishedAt := outcome.FinishedAt.UTC()
	settled.FinishedAt = &finishedAt
	if err := r.putRoll(ctx, &settled); err != nil {
		return err
	}

	w.Settle(roll, outcome)
	body, err := r.encode(w)
	if err != nil {
		return err
	}
	if _, err := r.watches.Put(ctx, key, body); err != nil {
		return fmt.Errorf("settle roll on %s: %w", watchID, err)
	}
	return nil
}

func (r *KVRepository) TouchChecked(ctx context.Context, watchID string, at time.Time, checkErr string) error {
	return r.mutate(ctx, watchID, func(w *domain.Watch) {
		utc := at.UTC()
		w.LastCheckedAt = &utc
		w.LastError = checkErr
	})
}

func (r *KVRepository) SetPinnedTag(ctx context.Context, watchID, tag string) error {
	return r.mutate(ctx, watchID, func(w *domain.Watch) {
		w.PinnedTag = tag
		w.UpdatedAt = time.Now().UTC()
	})
}

func (r *KVRepository) AdoptCurrentTag(ctx context.Context, watchID, tag string) error {
	return r.mutate(ctx, watchID, func(w *domain.Watch) {
		if w.Status == domain.StatusRolling {
			return
		}
		w.CurrentTag = tag
		w.UpdatedAt = time.Now().UTC()
	})
}

// Release lifts a hold and a pin, and clears the error that came with them.
func (r *KVRepository) Release(ctx context.Context, watchID string) error {
	return r.mutate(ctx, watchID, func(w *domain.Watch) {
		w.HeldTag = ""
		w.PinnedTag = ""
		w.LastError = ""
		w.Status = domain.StatusIdle
		w.UpdatedAt = time.Now().UTC()
	})
}

// ResetOrphans returns a rolling watch to idle when no roll of its is still
// waiting to settle — a process that died mid-roll leaves the watch claimed
// forever otherwise, and nothing would ever check it again.
func (r *KVRepository) ResetOrphans(ctx context.Context, reason string) (int, error) {
	watches, err := r.all(ctx, ">")
	if err != nil {
		return 0, err
	}
	pending, err := r.pendingByWatch(ctx)
	if err != nil {
		return 0, err
	}
	reset := 0
	for _, w := range watches {
		if w.Status != domain.StatusRolling || pending[w.ID] {
			continue
		}
		if err := r.mutate(ctx, w.ID, func(w *domain.Watch) {
			w.Status = domain.StatusFailed
			w.LastError = reason
			w.UpdatedAt = time.Now().UTC()
		}); err != nil {
			return reset, err
		}
		reset++
	}
	return reset, nil
}

// ListRolls returns a watch's rolls, newest first.
func (r *KVRepository) ListRolls(ctx context.Context, watchID string, limit int) ([]*domain.Roll, error) {
	// ">" and not "*": the key is "<watchID>.<nanos>.<rollID>", and a single
	// "*" matches ONE token, so it would never reach a roll.
	rolls, err := r.rollsMatching(ctx, watchID+".>", nil)
	if err != nil {
		return nil, err
	}
	sort.Slice(rolls, func(i, j int) bool { return rolls[i].CreatedAt.After(rolls[j].CreatedAt) })
	if limit > 0 && len(rolls) > limit {
		rolls = rolls[:limit]
	}
	return rolls, nil
}

// ListPendingRolls returns applied-but-unsettled rolls since a cutoff, oldest
// first — the ones whose node outcome is still owed.
func (r *KVRepository) ListPendingRolls(ctx context.Context, since time.Time) ([]*domain.Roll, error) {
	rolls, err := r.rollsMatching(ctx, ">", func(roll *domain.Roll) bool {
		return roll.Result == domain.RollResultApplied && roll.FinishedAt == nil && roll.CreatedAt.After(since)
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(rolls, func(i, j int) bool { return rolls[i].CreatedAt.Before(rolls[j].CreatedAt) })
	return rolls, nil
}

func (r *KVRepository) pendingByWatch(ctx context.Context) (map[string]bool, error) {
	rolls, err := r.rollsMatching(ctx, ">", func(roll *domain.Roll) bool {
		return roll.Result == domain.RollResultApplied && roll.FinishedAt == nil
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(rolls))
	for _, roll := range rolls {
		out[roll.WatchID] = true
	}
	return out, nil
}

// mutate applies fn to a watch and writes it back under the revision it was
// read at, so a concurrent write loses rather than being silently overwritten.
func (r *KVRepository) mutate(ctx context.Context, watchID string, fn func(*domain.Watch)) error {
	key, entry, w, err := r.findEntry(ctx, watchID)
	if err != nil {
		return err
	}
	if w == nil {
		return domain.ErrNotFound
	}
	fn(w)
	body, err := r.encode(w)
	if err != nil {
		return err
	}
	if _, err := r.watches.Update(ctx, key, body, entry.Revision()); err != nil {
		return fmt.Errorf("update watch %s: %w", watchID, err)
	}
	return nil
}

// findEntry locates a watch by id, returning its key and the entry it was read
// from so a caller can write back under that revision.
func (r *KVRepository) findEntry(ctx context.Context, watchID string) (string, jetstream.KeyValueEntry, *domain.Watch, error) {
	keys, err := listKeys(ctx, r.watches, ">")
	if err != nil {
		return "", nil, nil, err
	}
	for _, key := range keys {
		entry, err := r.watches.Get(ctx, key)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			continue
		}
		if err != nil {
			return "", nil, nil, fmt.Errorf("get %s: %w", key, err)
		}
		w, err := r.decode(entry.Value())
		if err != nil {
			return "", nil, nil, err
		}
		if w.ID == watchID {
			return key, entry, w, nil
		}
	}
	return "", nil, nil, nil
}

func (r *KVRepository) all(ctx context.Context, filter string) ([]*domain.Watch, error) {
	keys, err := listKeys(ctx, r.watches, filter)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.Watch, 0, len(keys))
	for _, key := range keys {
		entry, err := r.watches.Get(ctx, key)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("get %s: %w", key, err)
		}
		w, err := r.decode(entry.Value())
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, nil
}

func (r *KVRepository) putRoll(ctx context.Context, roll *domain.Roll) error {
	body, err := json.Marshal(roll)
	if err != nil {
		return fmt.Errorf("marshal roll: %w", err)
	}
	if _, err := r.rolls.Put(ctx, rollKey(roll), body); err != nil {
		return fmt.Errorf("put roll %s: %w", roll.ID, err)
	}
	return nil
}

func (r *KVRepository) rollsMatching(ctx context.Context, filter string, keep func(*domain.Roll) bool) ([]*domain.Roll, error) {
	keys, err := listKeys(ctx, r.rolls, filter)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.Roll, 0, len(keys))
	for _, key := range keys {
		entry, err := r.rolls.Get(ctx, key)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("get %s: %w", key, err)
		}
		var roll domain.Roll
		if uerr := json.Unmarshal(entry.Value(), &roll); uerr != nil {
			return nil, fmt.Errorf("decode roll: %w", uerr)
		}
		if keep != nil && !keep(&roll) {
			continue
		}
		out = append(out, &roll)
	}
	return out, nil
}

func watchKey(stackID, watchID string) string { return stackID + "." + watchID }

// rollKey puts the creation time in the key so the journal sorts by time, and
// keeps the id so two rolls created in the same nanosecond stay distinct.
func rollKey(roll *domain.Roll) string {
	return fmt.Sprintf("%s.%020d.%s", roll.WatchID, roll.CreatedAt.UTC().UnixNano(), roll.ID)
}

func listKeys(ctx context.Context, kv jetstream.KeyValue, filter string) ([]string, error) {
	lister, err := kv.ListKeysFiltered(ctx, filter)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("list keys %q: %w", filter, err)
	}
	var out []string
	for key := range lister.Keys() {
		out = append(out, key)
	}
	return out, nil
}

var _ domain.Repository = (*KVRepository)(nil)
