package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/lalternativefabrique/sklp/reconcile/deployconfig/domain"
)

// kv_repository.go is the JetStream KV adapter behind domain.Repository, for
// the reconciler running on the node (ADR-014). It stores exactly what the
// Postgres one does and answers the same port, so which backing store a
// deployment lives in is a wiring decision, not a domain one.
//
// Nothing here is irreplaceable: Git declares the topology and the node
// reports what runs, so this bucket holds the derived state that lets a
// reconcile skip an unchanged manifest. Losing it costs one redundant apply,
// never a wrong decision — which is why a KV with no backups is the right
// shape and a database is not.

// DeploymentsBucket is the KV bucket name this adapter reads and writes.
const DeploymentsBucket = "DEPLOYMENTS"

// KVRepository implements domain.Repository over a JetStream KV bucket.
//
// The key is "<stackID>.<space>", the identity migration 000094 settled on: a
// space belongs to one deployment, and it stays unique whichever repo declares
// it. That makes FindBySpace a direct Get and gives FindByStackID a prefix to
// filter on, without a secondary index to keep consistent.
type KVRepository struct {
	kv jetstream.KeyValue
}

func NewKVRepository(kv jetstream.KeyValue) *KVRepository {
	return &KVRepository{kv: kv}
}

// deploymentKey builds the bucket key. NATS keys are dot-separated, and a
// space name may not contain a dot (spacemanager rejects one), so no escaping
// is needed for the segment that could carry user input.
func deploymentKey(stackID, space string) string {
	return stackID + "." + space
}

func (r *KVRepository) Save(ctx context.Context, d *domain.Deployment) error {
	return r.put(ctx, d)
}

// Update writes the same key Save does. The two are distinct on the Postgres
// side (INSERT vs UPDATE); a KV put is create-or-replace, so they converge —
// keeping both is what lets one port serve both stores.
func (r *KVRepository) Update(ctx context.Context, d *domain.Deployment) error {
	return r.put(ctx, d)
}

func (r *KVRepository) put(ctx context.Context, d *domain.Deployment) error {
	body, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("marshal deployment: %w", err)
	}
	if _, err := r.kv.Put(ctx, deploymentKey(d.StackID, d.Space), body); err != nil {
		return fmt.Errorf("put deployment %s/%s: %w", d.StackID, d.Space, err)
	}
	return nil
}

// FindBySpace returns nil (not an error) when the space has no deployment,
// matching the Postgres adapter: the reconcile handler reads nil as "first
// deploy" and creates the row.
func (r *KVRepository) FindBySpace(ctx context.Context, stackID, space string) (*domain.Deployment, error) {
	entry, err := r.kv.Get(ctx, deploymentKey(stackID, space))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get deployment %s/%s: %w", stackID, space, err)
	}
	return decodeDeployment(entry.Value())
}

func (r *KVRepository) FindByStackID(ctx context.Context, stackID string) ([]*domain.Deployment, error) {
	return r.collect(ctx, stackID+".*", nil)
}

// FindApplied filters in memory rather than in the store. A KV has no query,
// and a node runs a handful of spaces, so scanning the bucket costs less than
// the index that would avoid it.
func (r *KVRepository) FindApplied(ctx context.Context) ([]*domain.Deployment, error) {
	return r.collect(ctx, ">", func(d *domain.Deployment) bool {
		return d.LastAppliedAt != nil
	})
}

// Delete removes a deployment by id. The key is (stack, space), so this scans
// to find which key holds that id — acceptable because nothing calls it today,
// and a delete that has to search is better than an index maintained for a
// path with no caller.
func (r *KVRepository) Delete(ctx context.Context, id string) error {
	keys, err := r.keys(ctx, ">")
	if err != nil {
		return err
	}
	for _, key := range keys {
		entry, err := r.kv.Get(ctx, key)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("get %s: %w", key, err)
		}
		d, err := decodeDeployment(entry.Value())
		if err != nil {
			return err
		}
		if d.ID == id {
			if err := r.kv.Delete(ctx, key); err != nil {
				return fmt.Errorf("delete %s: %w", key, err)
			}
			return nil
		}
	}
	return nil
}

// collect reads every key matching filter, keeping the ones keep accepts (nil
// keeps all). A key that vanishes between the listing and the read is skipped:
// concurrent deletion is not this reader's error to report.
func (r *KVRepository) collect(ctx context.Context, filter string, keep func(*domain.Deployment) bool) ([]*domain.Deployment, error) {
	keys, err := r.keys(ctx, filter)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.Deployment, 0, len(keys))
	for _, key := range keys {
		entry, err := r.kv.Get(ctx, key)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("get %s: %w", key, err)
		}
		d, err := decodeDeployment(entry.Value())
		if err != nil {
			return nil, err
		}
		if keep != nil && !keep(d) {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

func (r *KVRepository) keys(ctx context.Context, filter string) ([]string, error) {
	lister, err := r.kv.ListKeysFiltered(ctx, filter)
	if err != nil {
		// An empty bucket reports no keys rather than an error.
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

func decodeDeployment(raw []byte) (*domain.Deployment, error) {
	var d domain.Deployment
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("decode deployment: %w", err)
	}
	return &d, nil
}

// SpaceOfKey reports the space a bucket key names, for callers reading the
// bucket without decoding every value.
func SpaceOfKey(key string) string {
	_, space, found := strings.Cut(key, ".")
	if !found {
		return ""
	}
	return space
}

var _ domain.Repository = (*KVRepository)(nil)
