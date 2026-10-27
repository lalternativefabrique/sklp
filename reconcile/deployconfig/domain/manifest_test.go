package domain

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const validDeployYAML = `
services:
  api:
    image: rg.fr-par.scw.cloud/skalpai/api:20260828-0900-abc
    port: 8080
    replicas: 2
    env:
      LOG_LEVEL: info
`

// stubSecretResolver returns a fixed map, or err if set. calls counts
// invocations, so a test can assert ParseManifest resolves AT MOST ONCE per
// call even when several ${VAR} references appear (see ParseManifest's doc
// on the lazy, single-fetch contract).
type stubSecretResolver struct {
	secrets map[string]string
	err     error
	calls   int
}

func (r *stubSecretResolver) ResolveSecrets(context.Context, string, string, string) (map[string]string, error) {
	r.calls++
	if r.err != nil {
		return nil, r.err
	}
	return r.secrets, nil
}

// TestParseManifestValid checks the happy path parses one service and
// normalises env into a sorted KEY=value slice.
func TestParseManifestValid(t *testing.T) {
	m, err := ParseManifest(context.Background(), []byte(validDeployYAML), nil, "", "")
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m.NodeID != "" || m.Space != "" {
		t.Fatalf("destination = %q/%q, want empty: only deploy-root.yaml names it", m.NodeID, m.Space)
	}
	if len(m.Services) != 1 {
		t.Fatalf("services = %d, want 1", len(m.Services))
	}
	s := m.Services[0]
	if s.Name != "api" || s.Image != "rg.fr-par.scw.cloud/skalpai/api:20260828-0900-abc" || s.Port != 8080 || s.Replicas != 2 {
		t.Fatalf("unexpected service: %+v", s)
	}
	if len(s.Env) != 1 || s.Env[0] != "LOG_LEVEL=info" {
		t.Fatalf("env = %v, want [LOG_LEVEL=info]", s.Env)
	}
}

func TestParseManifestRequiresAWorkload(t *testing.T) {
	_, err := ParseManifest(context.Background(), []byte("jobs: {}\n"), nil, "", "")
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
}

const jobOnlyYAML = `
registries:
  rg.fr-par.scw.cloud:
    username: nologin
    password: ${SCW_SECRET_KEY}
jobs:
  restore-check:
    image: rg.fr-par.scw.cloud/platform-ops/postgres-pgbackrest:20260923-1510-be49997204a8
    schedule: "0 5 * * *"
`

func TestParseManifestAcceptsAJobOnlySpace(t *testing.T) {
	var asked string
	m, err := ParseManifestWith(context.Background(), []byte(jobOnlyYAML), ParseOptions{
		RegistryPassword: func(_ context.Context, imageRepo string) (string, bool) {
			asked = imageRepo
			return "from-stack", true
		},
	})
	if err != nil {
		t.Fatalf("ParseManifestWith: %v", err)
	}
	if asked != "rg.fr-par.scw.cloud/platform-ops/postgres-pgbackrest" {
		t.Fatalf("registry credential looked up for %q, want the job's image repository", asked)
	}
	if m.Vars["SCW_SECRET_KEY"] != "from-stack" {
		t.Fatalf("vars = %v", m.Vars)
	}
}

// TestParseManifestRequiresImage: a service with no image cannot ever be
// applied — mirrors spacemanager.DeployConfig.validate's same rule.
func TestParseManifestRequiresImage(t *testing.T) {
	raw := "services:\n  api:\n    port: 8080\n"
	_, err := ParseManifest(context.Background(), []byte(raw), nil, "", "")
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
}

// TestParseManifestRejectsInvalidServiceName mirrors the node's own
// serviceNameRE (apps/sklp/spacemanager/control/apply_linux.go) — rejecting
// it here surfaces the error in Skalpai's log instead of an opaque 4xx from
// the node.
func TestParseManifestRejectsInvalidServiceName(t *testing.T) {
	raw := "services:\n  API_bad:\n    image: img:tag\n"
	_, err := ParseManifest(context.Background(), []byte(raw), nil, "", "")
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
}

// TestParseManifestRejectsReplicasNegative mirrors
// spacemanager.DeployConfig.validate's own "replicas must be >= 0" rule.
func TestParseManifestRejectsReplicasNegative(t *testing.T) {
	raw := "services:\n  api:\n    image: img:tag\n    replicas: -1\n"
	_, err := ParseManifest(context.Background(), []byte(raw), nil, "", "")
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
}

// TestParseManifestRejectsSecretReferenceWithoutResolver: a ${VAR} in a
// service's env must be refused when no SecretResolver is wired, never
// silently sent to the node as the literal string "${VAR}" — this is the
// nil-secrets branch of ParseManifest; see manifest_test.go's
// TestParseManifestResolves* for the case where a resolver IS wired (issue
// 206ec5bd's step 4).
func TestParseManifestRejectsSecretReferenceWithoutResolver(t *testing.T) {
	raw := `
services:
  api:
    image: img:tag
    env:
      S3_SECRET_KEY: ${S3_SECRET_KEY}
`
	_, err := ParseManifest(context.Background(), []byte(raw), nil, "", "")
	if !errors.Is(err, ErrSecretsNotSupported) {
		t.Fatalf("err = %v, want ErrSecretsNotSupported", err)
	}
	if !strings.Contains(err.Error(), "S3_SECRET_KEY") {
		t.Fatalf("err = %v, want it to name the offending key", err)
	}
}

// TestParseManifestMalformedYAML: garbage input is a parse error, not a
// panic.
func TestParseManifestMalformedYAML(t *testing.T) {
	if _, err := ParseManifest(context.Background(), []byte("not: valid: yaml: ["), nil, "", ""); err == nil {
		t.Fatal("expected an error for malformed YAML")
	}
}

// TestManifestHashDeterministic: two parses of the same bytes must hash
// identically — Hash's whole purpose is telling "nothing changed" from
// "re-apply" across two independent parses of the same content.
func TestManifestHashDeterministic(t *testing.T) {
	a, err := ParseManifest(context.Background(), []byte(validDeployYAML), nil, "", "")
	if err != nil {
		t.Fatalf("ParseManifest a: %v", err)
	}
	b, err := ParseManifest(context.Background(), []byte(validDeployYAML), nil, "", "")
	if err != nil {
		t.Fatalf("ParseManifest b: %v", err)
	}
	if a.Hash() != b.Hash() {
		t.Fatalf("Hash() differs for identical manifests: %q vs %q", a.Hash(), b.Hash())
	}
}

// TestManifestHashChangesWithContent: a real content change (image tag)
// must change the hash — the whole point of the fingerprint.
func TestManifestHashChangesWithContent(t *testing.T) {
	a, err := ParseManifest(context.Background(), []byte(validDeployYAML), nil, "", "")
	if err != nil {
		t.Fatalf("ParseManifest a: %v", err)
	}
	changed := strings.Replace(validDeployYAML, "20260828-0900-abc", "20260828-1200-def", 1)
	b, err := ParseManifest(context.Background(), []byte(changed), nil, "", "")
	if err != nil {
		t.Fatalf("ParseManifest b: %v", err)
	}
	if a.Hash() == b.Hash() {
		t.Fatal("Hash() did not change when the image tag changed")
	}
}

// A manifest still carrying the retired target block parses fine: the node
// ignores unknown keys, and deploy-root.yaml alone names the destination.
// The hash is the file as committed, so that stray key is part of it.
func TestParseManifestToleratesStrayTargetKey(t *testing.T) {
	a, err := ParseManifest(context.Background(), []byte(validDeployYAML), nil, "", "")
	if err != nil {
		t.Fatalf("ParseManifest a: %v", err)
	}
	stray := "target:\n  node_id: node-2\n  space: staging\n" + validDeployYAML
	b, err := ParseManifest(context.Background(), []byte(stray), nil, "", "")
	if err != nil {
		t.Fatalf("ParseManifest with a stray target key: %v", err)
	}
	if b.NodeID != "" || b.Space != "" {
		t.Fatalf("destination = %q/%q, want empty: target is not read from the file", b.NodeID, b.Space)
	}
	if a.Hash() == b.Hash() {
		t.Fatal("Hash() must cover the file as committed, stray keys included")
	}
}

// TestParseManifestResolvesSecretReference is the load-bearing happy path
// for issue 206ec5bd's step 4: a ${VAR} in a service's env is substituted
// with the resolver's plaintext, not rejected.
func TestParseManifestResolvesSecretReference(t *testing.T) {
	raw := `
services:
  api:
    image: img:tag
    env:
      S3_SECRET_KEY: ${S3_SECRET_KEY}
      LOG_LEVEL: info
`
	resolver := &stubSecretResolver{secrets: map[string]string{"S3_SECRET_KEY": "s3cr3t-value"}}
	m, err := ParseManifest(context.Background(), []byte(raw), resolver, "proj-1", "user-1")
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if resolver.calls != 1 {
		t.Fatalf("ResolveSecrets calls = %d, want 1", resolver.calls)
	}
	env := m.Services[0].Env
	want := []string{"LOG_LEVEL=info", "S3_SECRET_KEY=s3cr3t-value"}
	if len(env) != len(want) || env[0] != want[0] || env[1] != want[1] {
		t.Fatalf("env = %v, want %v", env, want)
	}
}

// TestParseManifestResolvesSecretsOnce: two ${VAR} references in the same
// (or different) services must trigger exactly one ResolveSecrets call —
// the lazy, single-fetch contract ParseManifest's doc commits to.
func TestParseManifestResolvesSecretsOnce(t *testing.T) {
	raw := `
services:
  api:
    image: img:tag
    env:
      A: ${A}
      B: ${B}
`
	resolver := &stubSecretResolver{secrets: map[string]string{"A": "a-val", "B": "b-val"}}
	if _, err := ParseManifest(context.Background(), []byte(raw), resolver, "proj-1", "user-1"); err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if resolver.calls != 1 {
		t.Fatalf("ResolveSecrets calls = %d, want 1 (one fetch for the whole manifest)", resolver.calls)
	}
}

// TestParseManifestSkipsResolveWhenNoSecretReferenced: a deploy.yaml with no
// ${VAR} at all must never call ResolveSecrets — a manifest with nothing to
// resolve should never touch project_secret.
func TestParseManifestSkipsResolveWhenNoSecretReferenced(t *testing.T) {
	resolver := &stubSecretResolver{}
	if _, err := ParseManifest(context.Background(), []byte(validDeployYAML), resolver, "proj-1", "user-1"); err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if resolver.calls != 0 {
		t.Fatalf("ResolveSecrets calls = %d, want 0 (no ${VAR} in this manifest)", resolver.calls)
	}
}

// TestParseManifestRejectsUnresolvedSecretName: a ${VAR} naming a secret the
// resolver did NOT return (missing, or not opted into allowed_runners:
// [deploy]) is ErrSecretNotFound — the name appears in the error, the value
// obviously never does since there is none to leak.
func TestParseManifestRejectsUnresolvedSecretName(t *testing.T) {
	raw := `
services:
  api:
    image: img:tag
    env:
      MISSING_SECRET: ${MISSING_SECRET}
`
	resolver := &stubSecretResolver{secrets: map[string]string{}}
	_, err := ParseManifest(context.Background(), []byte(raw), resolver, "proj-1", "user-1")
	if !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("err = %v, want ErrSecretNotFound", err)
	}
	if !strings.Contains(err.Error(), "MISSING_SECRET") {
		t.Fatalf("err = %v, want it to name the missing secret", err)
	}
}

// TestParseManifestPropagatesResolverTransportError: a resolver failure
// (e.g. the project_secret call itself errored) must surface as a distinct
// error, not be swallowed or misreported as ErrSecretNotFound.
func TestParseManifestPropagatesResolverTransportError(t *testing.T) {
	raw := `
services:
  api:
    image: img:tag
    env:
      A: ${A}
`
	resolver := &stubSecretResolver{err: errors.New("project not owned")}
	_, err := ParseManifest(context.Background(), []byte(raw), resolver, "proj-1", "user-1")
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, ErrSecretNotFound) {
		t.Fatal("a resolver transport error must not be reported as ErrSecretNotFound")
	}
}

// TestParseManifestNeverMutatesRawEnvMap guards the copy-before-write fix in
// ParseManifest's substitution loop: resolving a secret for one service must
// never corrupt another service's (or the same service's) unresolved env
// entries via a shared map reference.
func TestParseManifestNeverMutatesRawEnvMap(t *testing.T) {
	raw := `
services:
  api:
    image: img:tag
    env:
      SECRET: ${SECRET}
      PLAIN: literal-value
`
	resolver := &stubSecretResolver{secrets: map[string]string{"SECRET": "resolved"}}
	m, err := ParseManifest(context.Background(), []byte(raw), resolver, "proj-1", "user-1")
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	env := m.Services[0].Env
	want := []string{"PLAIN=literal-value", "SECRET=resolved"}
	if len(env) != len(want) || env[0] != want[0] || env[1] != want[1] {
		t.Fatalf("env = %v, want %v", env, want)
	}
}

// A reference is rarely the whole value: DATABASE_URL embeds one in a
// connection string. Slicing "${" and "}" off the value treated the entire
// string as the secret name and looked up
// "stgres://app:${POSTGRES_PASSWORD}@postgres:5432/app?sslmode=disabl",
// which failed every reconciliation of a repo with a database URL.
func TestParseManifestResolvesSecretEmbeddedInValue(t *testing.T) {
	raw := `
services:
  core:
    image: img:tag
    env:
      DATABASE_URL: postgres://app:${POSTGRES_PASSWORD}@postgres:5432/app?sslmode=disable
`
	resolver := &stubSecretResolver{secrets: map[string]string{"POSTGRES_PASSWORD": "p4ss"}}
	m, err := ParseManifest(context.Background(), []byte(raw), resolver, "proj-1", "user-1")
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	want := "DATABASE_URL=postgres://app:p4ss@postgres:5432/app?sslmode=disable"
	if got := m.Services[0].Env[0]; got != want {
		t.Fatalf("env = %q, want %q", got, want)
	}
}

// Several references in one value must all be substituted.
func TestParseManifestResolvesMultipleRefsInOneValue(t *testing.T) {
	raw := `
services:
  core:
    image: img:tag
    env:
      DSN: postgres://${DB_USER}:${DB_PASS}@postgres:5432/app
`
	resolver := &stubSecretResolver{secrets: map[string]string{"DB_USER": "app", "DB_PASS": "p4ss"}}
	m, err := ParseManifest(context.Background(), []byte(raw), resolver, "proj-1", "user-1")
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	want := "DSN=postgres://app:p4ss@postgres:5432/app"
	if got := m.Services[0].Env[0]; got != want {
		t.Fatalf("env = %q, want %q", got, want)
	}
}

// An unknown name inside a larger value must be reported by NAME, not by the
// mangled string the old slicing produced.
func TestParseManifestReportsMissingEmbeddedSecretByName(t *testing.T) {
	raw := `
services:
  core:
    image: img:tag
    env:
      DATABASE_URL: postgres://app:${NOPE}@postgres:5432/app
`
	resolver := &stubSecretResolver{secrets: map[string]string{"OTHER": "x"}}
	_, err := ParseManifest(context.Background(), []byte(raw), resolver, "proj-1", "user-1")
	if err == nil {
		t.Fatal("want an error for an unresolved secret")
	}
	if !strings.Contains(err.Error(), `"NOPE"`) {
		t.Fatalf("error must name the secret NOPE, got: %v", err)
	}
}

// A hostname or a model name is not a secret, but a manifest references it the
// same way. Without config.yaml those could not resolve at all, and core
// failed the reconciliation calling them missing secrets.
func TestParseManifestResolvesFromPlainConfig(t *testing.T) {
	raw := `
services:
  core:
    image: img:tag
    env:
      LALTER_HOST: ${LALTER_HOST}
`
	m, err := ParseManifestWithConfig(context.Background(), []byte(raw),
		map[string]string{"LALTER_HOST": "altfab.fr"}, nil, "proj-1", "user-1")
	if err != nil {
		t.Fatalf("ParseManifestWithConfig: %v", err)
	}
	if got := m.Services[0].Env[0]; got != "LALTER_HOST=altfab.fr" {
		t.Fatalf("env = %q, want the plain value substituted", got)
	}
}

// A name in both places is a secret: the plain twin must not override it,
// or moving a value into the store would silently keep shipping the old one.
func TestParseManifestPrefersTheSecretStoreOverPlainConfig(t *testing.T) {
	raw := `
services:
  core:
    image: img:tag
    env:
      TOKEN: ${TOKEN}
`
	resolver := &stubSecretResolver{secrets: map[string]string{"TOKEN": "from-store"}}
	m, err := ParseManifestWithConfig(context.Background(), []byte(raw),
		map[string]string{"TOKEN": "from-config"}, resolver, "proj-1", "user-1")
	if err != nil {
		t.Fatalf("ParseManifestWithConfig: %v", err)
	}
	if got := m.Services[0].Env[0]; got != "TOKEN=from-store" {
		t.Fatalf("env = %q, want the secret store to win", got)
	}
}

// Neither source has it: the error must still name the value.
func TestParseManifestReportsAValueNeitherSourceHas(t *testing.T) {
	raw := `
services:
  core:
    image: img:tag
    env:
      NOPE: ${NOPE}
`
	_, err := ParseManifestWithConfig(context.Background(), []byte(raw),
		map[string]string{"OTHER": "x"}, nil, "proj-1", "user-1")
	if err == nil || !strings.Contains(err.Error(), "NOPE") {
		t.Fatalf("err = %v, want it to name NOPE", err)
	}
}

const routedDeployYAML = `
services:
  postgres:
    image: postgres:17
    env:
      POSTGRES_PASSWORD: ${DB_PASS}
  web:
    image: img:tag
jobs:
  migrate:
    image: migrate:1
    command: ["postgres://app:${DB_PASS}@postgres:5432/app", "up"]
spaces:
  app-production:
    gateway:
      routes:
        - service: web
          host: ${APP_HOST}
`

func TestParseManifestCollectsEveryReferencedVar(t *testing.T) {
	resolver := &stubSecretResolver{secrets: map[string]string{"DB_PASS": "p4ss"}}
	m, err := ParseManifestWithConfig(context.Background(), []byte(routedDeployYAML), map[string]string{"APP_HOST": "example.org"}, resolver, "proj-1", "user-1")
	if err != nil {
		t.Fatalf("ParseManifestWithConfig: %v", err)
	}
	if m.Vars["DB_PASS"] != "p4ss" || m.Vars["APP_HOST"] != "example.org" || len(m.Vars) != 2 {
		t.Fatalf("Vars = %v, want DB_PASS and APP_HOST", m.Vars)
	}
	if string(m.Raw) != routedDeployYAML {
		t.Fatal("Raw must be the file as committed, never the expanded text")
	}
	if resolver.calls != 1 {
		t.Fatalf("resolver calls = %d, want 1", resolver.calls)
	}
}

func TestParseManifestRejectsUnresolvedRouteHost(t *testing.T) {
	resolver := &stubSecretResolver{secrets: map[string]string{"DB_PASS": "p4ss"}}
	_, err := ParseManifestWithConfig(context.Background(), []byte(routedDeployYAML), nil, resolver, "proj-1", "user-1")
	if !errors.Is(err, ErrSecretNotFound) || !strings.Contains(err.Error(), "APP_HOST") {
		t.Fatalf("err = %v, want ErrSecretNotFound naming APP_HOST", err)
	}
}

func TestManifestHashChangesWithARoute(t *testing.T) {
	resolver := &stubSecretResolver{secrets: map[string]string{"DB_PASS": "p4ss"}}
	plain := map[string]string{"APP_HOST": "example.org"}
	a, err := ParseManifestWithConfig(context.Background(), []byte(routedDeployYAML), plain, resolver, "p", "u")
	if err != nil {
		t.Fatal(err)
	}
	moved := strings.Replace(routedDeployYAML, "host: ${APP_HOST}", "host: other.example.org", 1)
	b, err := ParseManifestWithConfig(context.Background(), []byte(moved), plain, resolver, "p", "u")
	if err != nil {
		t.Fatal(err)
	}
	if a.Hash() == b.Hash() {
		t.Fatal("a route change must change the hash, or the reconcile never re-applies it")
	}
}

func TestStackOf(t *testing.T) {
	for in, want := range map[string]string{
		".sklp/prod/deploy.yaml":    "prod",
		".sklp/staging/deploy.yaml": "staging",
		".sklp/deploy.yaml":         "",
		"deploy.yaml":               "",
	} {
		if got := StackOf(in); got != want {
			t.Errorf("StackOf(%q) = %q, want %q", in, got, want)
		}
	}
}

const registryBlockYAML = `
registries:
  rg.fr-par.scw.cloud:
    username: nologin
    password: ${SCW_SECRET_KEY}
services:
  api:
    image: rg.fr-par.scw.cloud/gate/kratos:20260914-0628-3e3c14ff03cb
    port: 4433
`

// The stack's registry credential, set once in the UI for its image watches,
// is what the reconcile pulls with when no store defines the block's variable.
func TestParseManifestTakesRegistryPasswordFromStackCredential(t *testing.T) {
	var asked string
	m, err := ParseManifestWith(context.Background(), []byte(registryBlockYAML), ParseOptions{
		RegistryPassword: func(_ context.Context, imageRepo string) (string, bool) {
			asked = imageRepo
			return "from-stack", true
		},
	})
	if err != nil {
		t.Fatalf("ParseManifestWith: %v", err)
	}
	if asked != "rg.fr-par.scw.cloud/gate/kratos" {
		t.Fatalf("asked for %q, want the image repository without its tag", asked)
	}
	if m.Vars["SCW_SECRET_KEY"] != "from-stack" {
		t.Fatalf("vars = %v", m.Vars)
	}
}

// A registry password never comes from the project store, even when it
// holds a value under that name: the Registry tab is its one source.
func TestParseManifestIgnoresTheStoreForARegistryVar(t *testing.T) {
	store := &stubSecretResolver{secrets: map[string]string{"SCW_SECRET_KEY": "from-store"}}
	m, err := ParseManifestWith(context.Background(), []byte(registryBlockYAML), ParseOptions{
		Resolver: legacyResolver{inner: store},
		RegistryPassword: func(context.Context, string) (string, bool) {
			return "from-stack", true
		},
	})
	if err != nil {
		t.Fatalf("ParseManifestWith: %v", err)
	}
	if m.Vars["SCW_SECRET_KEY"] != "from-stack" {
		t.Fatalf("vars = %v", m.Vars)
	}
	if store.calls != 0 {
		t.Fatalf("the store was consulted %d time(s) for a registry variable", store.calls)
	}
	_, err = ParseManifestWith(context.Background(), []byte(registryBlockYAML), ParseOptions{
		Resolver: legacyResolver{inner: store},
	})
	if err == nil {
		t.Fatal("without a stack credential the store must not stand in")
	}
}

// A stack with no matching credential fails the way an unresolved secret
// always did, so a push never applies a manifest that would pull anonymously.
func TestParseManifestRejectsRegistryVarNoCredentialCovers(t *testing.T) {
	_, err := ParseManifestWith(context.Background(), []byte(registryBlockYAML), ParseOptions{
		RegistryPassword: func(context.Context, string) (string, bool) { return "", false },
	})
	if err == nil {
		t.Fatal("want an error")
	}
}

// The production case: the project store answers, but without the registry
// variable (the secret was deleted there). The stack credential matched by
// the image's prefix is what resolves it.
func TestParseManifestStackCredentialCoversVarTheStoreLacks(t *testing.T) {
	store := &stubSecretResolver{secrets: map[string]string{"OTHER": "x"}}
	m, err := ParseManifestWith(context.Background(), []byte(registryBlockYAML), ParseOptions{
		Resolver:  legacyResolver{inner: store},
		ProjectID: "d059f686",
		RegistryPassword: func(_ context.Context, imageRepo string) (string, bool) {
			if imageRepo == "rg.fr-par.scw.cloud/gate/kratos" {
				return "stack-key", true
			}
			return "", false
		},
	})
	if err != nil {
		t.Fatalf("ParseManifestWith: %v", err)
	}
	if m.Vars["SCW_SECRET_KEY"] != "stack-key" {
		t.Fatalf("vars = %v", m.Vars)
	}
}

// A service may hand the registry key to a container too (a tool that pulls
// images itself): the env reference resolves from the same stack credential,
// with no store needed.
func TestParseManifestServiceEnvTakesRegistryVarFromStackCredential(t *testing.T) {
	raw := registryBlockYAML + "    env:\n      REG_PW: ${SCW_SECRET_KEY}\n"
	m, err := ParseManifestWith(context.Background(), []byte(raw), ParseOptions{
		RegistryPassword: func(context.Context, string) (string, bool) { return "stack-key", true },
	})
	if err != nil {
		t.Fatalf("ParseManifestWith: %v", err)
	}
	if env := m.Services[0].Env; len(env) != 1 || env[0] != "REG_PW=stack-key" {
		t.Fatalf("env = %v", env)
	}
	if m.Vars["SCW_SECRET_KEY"] != "stack-key" {
		t.Fatalf("vars = %v", m.Vars)
	}
}
