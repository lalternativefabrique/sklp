package secrets

import (
	"context"
	"fmt"

	"github.com/lalternativefabrique/sklp/reconcile/deployconfig/domain"
)

// Provider reads secrets from one backing store. Everything a store needs to
// be reached — a region, an endpoint, credentials — arrives in the store
// declaration, so a provider holds no configuration of its own and one
// instance serves every store of its kind.
type Provider interface {
	// Fetch returns {ref.Name: plaintext} for the refs it was given. A name
	// absent from the map was not found; the provider does not distinguish
	// missing from unreadable, which is what project_secret's own endpoint
	// already refuses to reveal.
	Fetch(ctx context.Context, store domain.SecretStore, req domain.SecretRequest, refs []domain.SecretRef) (map[string]string, error)
}

// ManifestResolver resolves each reference from the store its manifest names,
// dispatching to the provider registered for that store's kind.
//
// References with no store, and every reference at all when a repo declares no
// manifest, go to the Skalpai provider — the store that was the only possible
// answer before a manifest could name another.
type ManifestResolver struct {
	skalpai   Provider
	providers map[string]Provider
}

func NewManifestResolver(skalpai Provider) *ManifestResolver {
	return &ManifestResolver{skalpai: skalpai, providers: map[string]Provider{}}
}

// Register adds a provider under the name a store's `provider:` field uses.
func (r *ManifestResolver) Register(name string, p Provider) *ManifestResolver {
	r.providers[name] = p
	return r
}

// Resolve implements domain.Resolver.
func (r *ManifestResolver) Resolve(ctx context.Context, req domain.SecretRequest) (map[string]string, error) {
	// No declared refs is the pre-manifest question — "this project's
	// deploy-readable secrets" — which only the Skalpai store can answer.
	if len(req.Refs) == 0 {
		return r.skalpai.Fetch(ctx, domain.SecretStore{Name: domain.ProviderSkalpai}, req, nil)
	}

	byStore := map[string][]domain.SecretRef{}
	for _, ref := range req.Refs {
		byStore[ref.Store] = append(byStore[ref.Store], ref)
	}

	out := make(map[string]string, len(req.Refs))
	for storeName, refs := range byStore {
		provider, store, err := r.providerFor(storeName, req)
		if err != nil {
			return nil, err
		}
		// One call per store, not per secret: a store is an authenticated
		// connection, and the manifest groups references precisely so several
		// secrets share it.
		values, err := provider.Fetch(ctx, store, req, refs)
		if err != nil {
			return nil, fmt.Errorf("store %q: %w", storeName, err)
		}
		for name, value := range values {
			out[name] = value
		}
	}
	return out, nil
}

func (r *ManifestResolver) providerFor(storeName string, req domain.SecretRequest) (Provider, domain.SecretStore, error) {
	if storeName == domain.ProviderSkalpai {
		return r.skalpai, domain.SecretStore{Name: domain.ProviderSkalpai}, nil
	}
	store, ok := req.Stores[storeName]
	if !ok {
		// ParseSecretsManifest rejects an undeclared store, so reaching here
		// means the request was built by something else.
		return nil, domain.SecretStore{}, fmt.Errorf("store %q is not declared", storeName)
	}
	provider, ok := r.providers[store.Provider]
	if !ok {
		return nil, domain.SecretStore{}, fmt.Errorf("store %q names provider %q, which this deployment does not support", storeName, store.Provider)
	}
	return provider, store, nil
}
