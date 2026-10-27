package secrets

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/reconcile/deployconfig/domain"
)

type stubProvider struct {
	values map[string]string
	calls  int
	seen   [][]domain.SecretRef
	err    error
}

func (p *stubProvider) Fetch(_ context.Context, _ domain.SecretStore, _ domain.SecretRequest, refs []domain.SecretRef) (map[string]string, error) {
	p.calls++
	p.seen = append(p.seen, refs)
	if p.err != nil {
		return nil, p.err
	}
	return p.values, nil
}

// A repo with no secrets.yaml sends no refs, which is the question the port
// asked before manifests existed. It must still reach the project store, or
// every deployment already in production stops resolving.
func TestResolveWithoutRefsFallsBackToTheProjectStore(t *testing.T) {
	skalpai := &stubProvider{values: map[string]string{"A": "1"}}
	r := NewManifestResolver(skalpai)

	got, err := r.Resolve(context.Background(), domain.SecretRequest{ProjectID: "p1"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got["A"] != "1" || skalpai.calls != 1 {
		t.Fatalf("got %v after %d calls, want the project store consulted once", got, skalpai.calls)
	}
}

// A store is an authenticated connection, and the manifest groups references
// so several secrets share it — one call per store, not per secret.
func TestResolveCallsEachProviderOncePerStore(t *testing.T) {
	skalpai := &stubProvider{values: map[string]string{"LOCAL": "l"}}
	scw := &stubProvider{values: map[string]string{"A": "1", "B": "2"}}
	r := NewManifestResolver(skalpai).Register("scaleway", scw)

	got, err := r.Resolve(context.Background(), domain.SecretRequest{
		Stores: map[string]domain.SecretStore{"scw": {Name: "scw", Provider: "scaleway"}},
		Refs: []domain.SecretRef{
			{Name: "A", Store: "scw", Key: "/a"},
			{Name: "B", Store: "scw", Key: "/b"},
			{Name: "LOCAL", Store: domain.ProviderSkalpai, Key: "LOCAL"},
		},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if scw.calls != 1 {
		t.Errorf("scaleway called %d times, want 1 for two refs sharing a store", scw.calls)
	}
	if len(scw.seen[0]) != 2 {
		t.Errorf("scaleway got %d refs, want both", len(scw.seen[0]))
	}
	if skalpai.calls != 1 {
		t.Errorf("skalpai called %d times, want 1", skalpai.calls)
	}
	for _, want := range []string{"A", "B", "LOCAL"} {
		if _, ok := got[want]; !ok {
			t.Errorf("%q missing from %v", want, got)
		}
	}
}

// Naming a provider this build does not have must say so, not silently
// resolve nothing and surface later as a missing secret.
func TestResolveRejectsAnUnsupportedProvider(t *testing.T) {
	r := NewManifestResolver(&stubProvider{})

	_, err := r.Resolve(context.Background(), domain.SecretRequest{
		Stores: map[string]domain.SecretStore{"v": {Name: "v", Provider: "vault"}},
		Refs:   []domain.SecretRef{{Name: "A", Store: "v"}},
	})
	if err == nil || !strings.Contains(err.Error(), "vault") {
		t.Fatalf("err = %v, want it to name the unsupported provider", err)
	}
}

// A store that cannot be read fails the reconcile rather than yielding a
// half-resolved manifest, which would deploy a service missing one value.
func TestResolveFailsWhenAProviderFails(t *testing.T) {
	boom := errors.New("credentials rejected")
	r := NewManifestResolver(&stubProvider{}).Register("scaleway", &stubProvider{err: boom})

	_, err := r.Resolve(context.Background(), domain.SecretRequest{
		Stores: map[string]domain.SecretStore{"scw": {Name: "scw", Provider: "scaleway"}},
		Refs:   []domain.SecretRef{{Name: "A", Store: "scw"}},
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the provider's error", err)
	}
	if !strings.Contains(err.Error(), "scw") {
		t.Errorf("err = %v, want it to name the store that failed", err)
	}
}
