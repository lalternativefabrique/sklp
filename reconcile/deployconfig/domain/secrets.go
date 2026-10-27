package domain

import (
	"context"
	"fmt"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

// secrets.go declares WHERE a deploy.yaml's ${VAR} values come from.
//
// Without a manifest a reconcile asks one question — "give me this project's
// deploy-readable secrets" — and the answer can only come from Skalpai's own
// store, because that question is only meaningful there. That is the coupling:
// not the call itself, but a port shaped so only one implementation can
// satisfy it.
//
// A manifest inverts it. The file names each secret and the store it lives in,
// so the resolver is asked to resolve REFERENCES, and a store is something a
// provider implements rather than something the port assumes. The shape is
// External Secrets': stores declared apart from the references that read them,
// so several secrets share one authenticated connection.
//
// Absent, nothing changes: every ${VAR} resolves from the Skalpai store as it
// always has, which is what keeps the deployments already in production
// reconciling untouched.

// SecretsPath returns the secrets manifest beside a deploy manifest, the same
// convention ConfigPathFor uses.
func SecretsPathFor(manifestPath string) string {
	return path.Join(path.Dir(manifestPath), "secrets.yaml")
}

// SecretsKind is the `kind:` a secrets.yaml must declare, so a file fetched
// from that path can be told apart from any other YAML.
const SecretsKind = "secrets"

// ProviderSkalpai is the built-in store: the project's own encrypted secrets,
// the only source before a manifest could name another. A reference with no
// store resolves here, which is why a repo with no manifest keeps working.
const ProviderSkalpai = "skalpai"

// SecretStore is an authenticated connection to one provider. Auth values are
// themselves ${VAR} references, resolved from the reconciler's environment
// rather than from a store — a store cannot bootstrap itself.
type SecretStore struct {
	Name     string            `yaml:"name"`
	Provider string            `yaml:"provider"`
	Auth     map[string]string `yaml:"auth,omitempty"`
	// Config carries provider-specific settings (a region, an endpoint, a
	// mount path) without this type growing a field per provider.
	Config map[string]string `yaml:"config,omitempty"`
}

// SecretRef binds one ${VAR} name to a key in a store.
type SecretRef struct {
	// Name is the ${VAR} a deploy.yaml references.
	Name string `yaml:"name"`
	// Store names a SecretStore, or is empty for the built-in Skalpai store.
	Store string `yaml:"store,omitempty"`
	// Key addresses the secret inside that store. Defaults to Name, so the
	// common case of a matching name states it once.
	Key string `yaml:"key,omitempty"`
	// Property selects one field of a structured secret (a JSON document
	// holding several values). Empty means the whole value.
	Property string `yaml:"property,omitempty"`
}

// SecretsManifest is a parsed secrets.yaml.
type SecretsManifest struct {
	Kind    string        `yaml:"kind"`
	Stores  []SecretStore `yaml:"stores,omitempty"`
	Secrets []SecretRef   `yaml:"secrets"`
}

// ErrNotASecretsFile marks a file at the secrets path that is not one.
var ErrNotASecretsFile = fmt.Errorf("not a %s file", SecretsKind)

// ParseSecretsManifest decodes and validates a secrets.yaml. Every reference
// must name a secret and a store that the file declares: a typo naming an
// undeclared store is rejected here rather than resolving to nothing and
// looking like a missing secret at apply time.
func ParseSecretsManifest(raw []byte) (*SecretsManifest, error) {
	var m SecretsManifest
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%w: parse secrets manifest: %v", ErrValidation, err)
	}
	if strings.TrimSpace(m.Kind) != SecretsKind {
		return nil, fmt.Errorf("%w (kind: %q)", ErrNotASecretsFile, m.Kind)
	}

	stores := map[string]bool{ProviderSkalpai: true}
	for i := range m.Stores {
		s := &m.Stores[i]
		s.Name = strings.TrimSpace(s.Name)
		s.Provider = strings.TrimSpace(s.Provider)
		if s.Name == "" {
			return nil, fmt.Errorf("%w: secrets manifest store %d declares no name", ErrValidation, i+1)
		}
		if s.Provider == "" {
			return nil, fmt.Errorf("%w: store %q declares no provider", ErrValidation, s.Name)
		}
		if stores[s.Name] {
			return nil, fmt.Errorf("%w: secrets manifest declares store %q twice", ErrValidation, s.Name)
		}
		stores[s.Name] = true
	}

	seen := map[string]bool{}
	for i := range m.Secrets {
		r := &m.Secrets[i]
		r.Name = strings.TrimSpace(r.Name)
		r.Store = strings.TrimSpace(r.Store)
		r.Key = strings.TrimSpace(r.Key)
		if r.Name == "" {
			return nil, fmt.Errorf("%w: secrets manifest entry %d declares no name", ErrValidation, i+1)
		}
		if r.Store == "" {
			r.Store = ProviderSkalpai
		}
		if !stores[r.Store] {
			return nil, fmt.Errorf("%w: secret %q names store %q, which the manifest does not declare", ErrValidation, r.Name, r.Store)
		}
		if r.Key == "" {
			r.Key = r.Name
		}
		if seen[r.Name] {
			return nil, fmt.Errorf("%w: secrets manifest declares %q twice", ErrValidation, r.Name)
		}
		seen[r.Name] = true
	}
	return &m, nil
}

// StoreByName indexes the manifest's stores. The built-in Skalpai store has no
// declaration and is absent from the map: a resolver reaching it needs no
// connection details.
func (m *SecretsManifest) StoreByName() map[string]SecretStore {
	out := make(map[string]SecretStore, len(m.Stores))
	for _, s := range m.Stores {
		out[s.Name] = s
	}
	return out
}

// SecretRequest is what a reconcile asks a resolver for: the references a
// manifest declared, plus the scope that authorizes reading them. Scope stays
// on the request rather than on each ref — every ref of one reconcile belongs
// to the same project, and the access log records a cause, not a per-secret
// justification.
type SecretRequest struct {
	ProjectID string
	UserID    string
	Reason    string
	Stores    map[string]SecretStore
	Refs      []SecretRef
}

// Resolver turns declared references into plaintext values, each from the
// store its reference names.
//
// It replaces a port that could only ever mean "this project's secrets from
// Skalpai's store": the caller now says which values it needs and where they
// live, so an implementation reading a provider satisfies it exactly as the
// in-process store does. A name absent from the returned map was not found —
// the resolver does not distinguish "never declared" from "not readable",
// preserving what project_secret's own endpoint already refuses to reveal.
type Resolver interface {
	Resolve(ctx context.Context, req SecretRequest) (map[string]string, error)
}
