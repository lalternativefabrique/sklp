package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/lalternativefabrique/sklp/reconcile/gateway/domain"
)

// kv_repository.go is the JetStream KV adapter behind the gateway's two
// repositories, for the reconciler running on the node (ADR-014).
//
// Unlike deployments, a node's row is NOT derived from Git: it carries the
// bearer token and the pinned certificate fingerprint, which exist nowhere
// else. On the node itself that matters less than it looks — a reconciler
// running on the machine it deploys reaches the supervisor over localhost and
// needs no credential for it — but the registry still records the routes and
// the identity an operator registered, so this bucket is the one piece of
// reconciler state worth persisting rather than rebuilding.

const (
	// NodesBucket holds one entry per registered node, keyed "<stackID>.<id>".
	NodesBucket = "NODES"
	// NodeNamesBucket maps "<stackID>.<name>" to a node id, so FindByName is a
	// pair of Gets rather than a scan. A node is looked up by name on every
	// reconcile (a deploy-root names it), which is what earns the index.
	NodeNamesBucket = "NODE_NAMES"
	// RoutesBucket holds one entry per route, keyed "<nodeID>.<id>".
	RoutesBucket = "ROUTES"
)

// KVNodeRepository implements domain.NodeRepository over JetStream KV.
type KVNodeRepository struct {
	nodes jetstream.KeyValue
	names jetstream.KeyValue
}

func NewKVNodeRepository(nodes, names jetstream.KeyValue) *KVNodeRepository {
	return &KVNodeRepository{nodes: nodes, names: names}
}

func scopedKey(scope, id string) string { return scope + "." + id }

func (r *KVNodeRepository) Save(ctx context.Context, node *domain.Node) error {
	return r.put(ctx, node)
}

func (r *KVNodeRepository) Update(ctx context.Context, node *domain.Node) error {
	// A rename leaves the old name pointing at this node, so the previous
	// entry is read and dropped before the new one is written: without it a
	// deploy-root naming the old name would keep resolving after the rename.
	if prev, err := r.FindByID(ctx, node.StackID, node.ID); err == nil && prev != nil && prev.Name != node.Name {
		if derr := r.names.Delete(ctx, scopedKey(prev.StackID, prev.Name)); derr != nil && !errors.Is(derr, jetstream.ErrKeyNotFound) {
			return fmt.Errorf("drop stale node name %q: %w", prev.Name, derr)
		}
	}
	return r.put(ctx, node)
}

func (r *KVNodeRepository) put(ctx context.Context, node *domain.Node) error {
	body, err := json.Marshal(node)
	if err != nil {
		return fmt.Errorf("marshal node: %w", err)
	}
	if _, err := r.nodes.Put(ctx, scopedKey(node.StackID, node.ID), body); err != nil {
		return fmt.Errorf("put node %s: %w", node.ID, err)
	}
	// The index is written second: a crash between the two leaves a node
	// reachable by id and invisible by name, which a re-save repairs. The
	// reverse would leave a name pointing at nothing.
	if _, err := r.names.Put(ctx, scopedKey(node.StackID, node.Name), []byte(node.ID)); err != nil {
		return fmt.Errorf("index node name %q: %w", node.Name, err)
	}
	return nil
}

func (r *KVNodeRepository) FindByID(ctx context.Context, stackID, id string) (*domain.Node, error) {
	entry, err := r.nodes.Get(ctx, scopedKey(stackID, id))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get node %s: %w", id, err)
	}
	return decodeNode(entry.Value())
}

func (r *KVNodeRepository) FindByName(ctx context.Context, stackID, name string) (*domain.Node, error) {
	entry, err := r.names.Get(ctx, scopedKey(stackID, name))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("resolve node name %q: %w", name, err)
	}
	return r.FindByID(ctx, stackID, string(entry.Value()))
}

func (r *KVNodeRepository) ListByStack(ctx context.Context, stackID string) ([]*domain.Node, error) {
	keys, err := listKeys(ctx, r.nodes, stackID+".*")
	if err != nil {
		return nil, err
	}
	out := make([]*domain.Node, 0, len(keys))
	for _, key := range keys {
		entry, err := r.nodes.Get(ctx, key)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("get %s: %w", key, err)
		}
		node, err := decodeNode(entry.Value())
		if err != nil {
			return nil, err
		}
		out = append(out, node)
	}
	return out, nil
}

func (r *KVNodeRepository) Delete(ctx context.Context, stackID, id string) error {
	// The name index is dropped first, using the node's own record to find it:
	// deleting the node first would leave no way to know which name to clear.
	node, err := r.FindByID(ctx, stackID, id)
	if err != nil {
		return err
	}
	if node != nil {
		if derr := r.names.Delete(ctx, scopedKey(stackID, node.Name)); derr != nil && !errors.Is(derr, jetstream.ErrKeyNotFound) {
			return fmt.Errorf("delete node name %q: %w", node.Name, derr)
		}
	}
	if derr := r.nodes.Delete(ctx, scopedKey(stackID, id)); derr != nil && !errors.Is(derr, jetstream.ErrKeyNotFound) {
		return fmt.Errorf("delete node %s: %w", id, derr)
	}
	return nil
}

// KVRouteRepository implements domain.RouteRepository over JetStream KV.
type KVRouteRepository struct {
	kv jetstream.KeyValue
}

func NewKVRouteRepository(kv jetstream.KeyValue) *KVRouteRepository {
	return &KVRouteRepository{kv: kv}
}

func (r *KVRouteRepository) Save(ctx context.Context, route *domain.Route) error {
	return r.put(ctx, route)
}

func (r *KVRouteRepository) Update(ctx context.Context, route *domain.Route) error {
	return r.put(ctx, route)
}

func (r *KVRouteRepository) put(ctx context.Context, route *domain.Route) error {
	body, err := json.Marshal(route)
	if err != nil {
		return fmt.Errorf("marshal route: %w", err)
	}
	if _, err := r.kv.Put(ctx, scopedKey(route.NodeID, route.ID), body); err != nil {
		return fmt.Errorf("put route %s: %w", route.ID, err)
	}
	return nil
}

func (r *KVRouteRepository) FindByID(ctx context.Context, nodeID, id string) (*domain.Route, error) {
	entry, err := r.kv.Get(ctx, scopedKey(nodeID, id))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get route %s: %w", id, err)
	}
	return decodeRoute(entry.Value())
}

// FindByHostPath scans the node's routes. It guards uniqueness on create, so
// it runs once per route added, not per request served — a scan of a node's
// handful of routes is cheaper than an index that must stay consistent with
// both host and path.
func (r *KVRouteRepository) FindByHostPath(ctx context.Context, nodeID, host, path string) (*domain.Route, error) {
	routes, err := r.ListByNode(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	for _, route := range routes {
		if route.Host == host && route.Path == path {
			return route, nil
		}
	}
	return nil, nil
}

func (r *KVRouteRepository) ListByNode(ctx context.Context, nodeID string) ([]*domain.Route, error) {
	keys, err := listKeys(ctx, r.kv, nodeID+".*")
	if err != nil {
		return nil, err
	}
	out := make([]*domain.Route, 0, len(keys))
	for _, key := range keys {
		entry, err := r.kv.Get(ctx, key)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("get %s: %w", key, err)
		}
		route, err := decodeRoute(entry.Value())
		if err != nil {
			return nil, err
		}
		out = append(out, route)
	}
	return out, nil
}

func (r *KVRouteRepository) Delete(ctx context.Context, nodeID, id string) error {
	if err := r.kv.Delete(ctx, scopedKey(nodeID, id)); err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return fmt.Errorf("delete route %s: %w", id, err)
	}
	return nil
}

// listKeys returns every key matching filter, reading an empty bucket as no
// keys rather than as a failure.
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

func decodeNode(raw []byte) (*domain.Node, error) {
	var n domain.Node
	if err := json.Unmarshal(raw, &n); err != nil {
		return nil, fmt.Errorf("decode node: %w", err)
	}
	return &n, nil
}

func decodeRoute(raw []byte) (*domain.Route, error) {
	var route domain.Route
	if err := json.Unmarshal(raw, &route); err != nil {
		return nil, fmt.Errorf("decode route: %w", err)
	}
	return &route, nil
}

var (
	_ domain.NodeRepository  = (*KVNodeRepository)(nil)
	_ domain.RouteRepository = (*KVRouteRepository)(nil)
)
