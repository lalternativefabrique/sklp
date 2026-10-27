package domain

import (
	"errors"
	"strings"
	"testing"
)

func TestSecretsPathForSitsBesideTheManifest(t *testing.T) {
	for manifest, want := range map[string]string{
		".sklp/prod/deploy.yaml": ".sklp/prod/secrets.yaml",
		".sklp/deploy.yaml":      ".sklp/secrets.yaml",
		"deploy.yaml":            "secrets.yaml",
	} {
		if got := SecretsPathFor(manifest); got != want {
			t.Errorf("SecretsPathFor(%q) = %q, want %q", manifest, got, want)
		}
	}
}

func TestParseSecretsManifestReadsStoresAndRefs(t *testing.T) {
	m, err := ParseSecretsManifest([]byte(`
kind: secrets
stores:
  - name: scw
    provider: scaleway
    auth:
      secretKey: ${SCW_SECRET_KEY}
    config:
      region: fr-par
secrets:
  - name: LALTER_LLM_API_KEY
    store: scw
    key: /platform-ops/altfab/llm-api-key
  - name: PG_S3_ACCESS_KEY
    store: scw
    key: /platform-ops/buckets/ovh/altfab
    property: access_key
`))
	if err != nil {
		t.Fatalf("ParseSecretsManifest: %v", err)
	}
	if len(m.Stores) != 1 || m.Stores[0].Provider != "scaleway" {
		t.Fatalf("stores = %+v, want one scaleway store", m.Stores)
	}
	if m.Stores[0].Config["region"] != "fr-par" {
		t.Errorf("config = %v, want region fr-par", m.Stores[0].Config)
	}
	if m.Secrets[1].Property != "access_key" {
		t.Errorf("property = %q, want access_key", m.Secrets[1].Property)
	}
	if got := m.StoreByName()["scw"].Name; got != "scw" {
		t.Errorf("StoreByName missed the declared store: %v", m.StoreByName())
	}
}

// A reference states its key only when it differs from the variable name, and
// omits the store when the value lives in Skalpai's own — the case a repo
// migrating one secret at a time writes.
func TestParseSecretsManifestDefaultsStoreAndKey(t *testing.T) {
	m, err := ParseSecretsManifest([]byte(`
kind: secrets
secrets:
  - name: BETTER_AUTH_SECRET
`))
	if err != nil {
		t.Fatalf("ParseSecretsManifest: %v", err)
	}
	ref := m.Secrets[0]
	if ref.Store != ProviderSkalpai {
		t.Errorf("store = %q, want the built-in %q", ref.Store, ProviderSkalpai)
	}
	if ref.Key != "BETTER_AUTH_SECRET" {
		t.Errorf("key = %q, want it to default to the name", ref.Key)
	}
}

// The built-in store needs no declaration, so naming it is always valid.
func TestParseSecretsManifestAcceptsTheBuiltInStoreByName(t *testing.T) {
	if _, err := ParseSecretsManifest([]byte(`
kind: secrets
secrets:
  - name: A
    store: skalpai
`)); err != nil {
		t.Fatalf("naming the built-in store must be valid: %v", err)
	}
}

// A store typo would otherwise resolve to nothing and surface as a missing
// secret at apply time, pointing at the wrong cause.
func TestParseSecretsManifestRejectsAnUndeclaredStore(t *testing.T) {
	_, err := ParseSecretsManifest([]byte(`
kind: secrets
secrets:
  - name: A
    store: vualt
`))
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
	if !strings.Contains(err.Error(), "vualt") {
		t.Errorf("err = %v, want it to name the undeclared store", err)
	}
}

func TestParseSecretsManifestRejectsIncompleteDeclarations(t *testing.T) {
	for name, raw := range map[string]string{
		"store with no name":     "kind: secrets\nstores:\n  - provider: scaleway\nsecrets:\n  - name: A\n",
		"store with no provider": "kind: secrets\nstores:\n  - name: scw\nsecrets:\n  - name: A\n",
		"secret with no name":    "kind: secrets\nsecrets:\n  - key: /a/b\n",
		"duplicate secret":       "kind: secrets\nsecrets:\n  - name: A\n  - name: A\n",
		"duplicate store":        "kind: secrets\nstores:\n  - name: scw\n    provider: scaleway\n  - name: scw\n    provider: vault\nsecrets:\n  - name: A\n",
	} {
		if _, err := ParseSecretsManifest([]byte(raw)); !errors.Is(err, ErrValidation) {
			t.Errorf("%s: err = %v, want ErrValidation", name, err)
		}
	}
}

// A store may not shadow the built-in: a reference naming "skalpai" would
// otherwise reach a provider the repo chose, not the project's own secrets.
func TestParseSecretsManifestRejectsShadowingTheBuiltInStore(t *testing.T) {
	_, err := ParseSecretsManifest([]byte(`
kind: secrets
stores:
  - name: skalpai
    provider: vault
secrets:
  - name: A
`))
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
}

// The kind guard keeps a mis-scoped fetch (a deploy.yaml served from the
// secrets path) from parsing as a manifest declaring nothing.
func TestParseSecretsManifestRejectsAForeignFile(t *testing.T) {
	_, err := ParseSecretsManifest([]byte("services:\n  api:\n    image: nginx\n"))
	if !errors.Is(err, ErrNotASecretsFile) {
		t.Fatalf("err = %v, want ErrNotASecretsFile", err)
	}
}
