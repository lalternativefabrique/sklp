package spacemanager

import (
	"os"
	"strings"
	"testing"
)

const envRefDeploy = `
version: 1
kind: deploy
name: app
services:
  api:
    image: example.com/api:1
    env:
      DATABASE_URL: ${DATABASE_URL}
      LOG_LEVEL: "debug"
      DSN: "host=${DB_HOST} port=5432"
  worker:
    image: example.com/worker:1
    env:
      API_TOKEN: ${API_TOKEN}
jobs:
  migrate:
    image: example.com/migrate:1
    env:
      MIGRATE_SECRET: ${MIGRATE_SECRET}
spaces:
  production:
    replicas: 1
    api:
      env:
        OVERRIDE_KEY: ${OVERRIDE_KEY}
`

func TestEnvVarRefs_CapturesEveryTemplatedValue(t *testing.T) {
	refs, err := envVarRefs([]byte(envRefDeploy))
	if err != nil {
		t.Fatalf("envVarRefs: %v", err)
	}
	want := map[string]string{
		"DATABASE_URL":   "${DATABASE_URL}",
		"API_TOKEN":      "${API_TOKEN}",
		"MIGRATE_SECRET": "${MIGRATE_SECRET}",
		"OVERRIDE_KEY":   "${OVERRIDE_KEY}",
		"DSN":            "host=${DB_HOST} port=5432",
	}
	for k, v := range want {
		if refs[k] != v {
			t.Errorf("refs[%q] = %q, want %q", k, refs[k], v)
		}
	}
	if _, ok := refs["LOG_LEVEL"]; ok {
		t.Error("a literal value must not be captured as a ref")
	}
}

// A composed value used to be expanded and persisted as a literal, which put
// the secret it embedded (a DB password inside a URL) in the on-disk manifest
// and in the manifest API. It is a template now: no secret ever leaves Parse
// except through EnvValues, which is transport-only.
func TestParseWith_ComposedValueTravelsAsTemplateAndValues(t *testing.T) {
	lookup := func(name string) (string, bool) {
		return map[string]string{"DATABASE_URL": "u", "API_TOKEN": "t", "MIGRATE_SECRET": "m", "OVERRIDE_KEY": "o", "DB_HOST": "db.internal"}[name], true
	}
	cfg, err := ParseWith([]byte(envRefDeploy), lookup)
	if err != nil {
		t.Fatal(err)
	}
	services, meta, err := cfg.Resolve("production", "")
	if err != nil {
		t.Fatal(err)
	}
	if meta.EnvRefs["DSN"] != "host=${DB_HOST} port=5432" || meta.EnvValues["DB_HOST"] != "db.internal" {
		t.Fatalf("refs=%v values=%v", meta.EnvRefs, meta.EnvValues)
	}
	stripped := StripEnvRefs(services, meta.EnvRefs)
	if joined := strings.Join(stripped[0].Env, " "); strings.Contains(joined, "db.internal") {
		t.Fatalf("composed secret survived the strip: %q", joined)
	}
	back := ResolveEnvRefsWith(stripped, meta.EnvRefs, func(name string) (string, bool) {
		v, ok := meta.EnvValues[name]
		return v, ok
	})
	if joined := strings.Join(back[0].Env, " "); !strings.Contains(joined, "DSN=host=db.internal port=5432") {
		t.Fatalf("composed value not rebuilt at launch: %q", joined)
	}
}

func TestExpandTemplate_ReadsLegacyNameAndTemplate(t *testing.T) {
	lookup := func(name string) (string, bool) {
		v, ok := map[string]string{"PW": "s3", "HOST": "db"}[name]
		return v, ok
	}
	if got := ExpandTemplate("PW", lookup); got != "s3" {
		t.Fatalf("legacy name = %q", got)
	}
	if got := ExpandTemplate("${PW}", lookup); got != "s3" {
		t.Fatalf("whole ref = %q", got)
	}
	if got := ExpandTemplate("postgres://app:${PW}@${HOST}/${SPACE}", lookup); got != "postgres://app:s3@db/${SPACE}" {
		t.Fatalf("composed = %q", got)
	}
	if got := ExpandTemplate("x=${MISSING}", lookup); got != "x=" {
		t.Fatalf("missing var must expand to empty, got %q", got)
	}
	if names := TemplateVarNames("${B}-${A}-${SPACE}-${A}"); strings.Join(names, ",") != "A,B" {
		t.Fatalf("names = %v", names)
	}
}

// TestStripEnvRefs_RemovesValuesKeepsKeys is the security-relevant case: the
// persisted manifest must not carry the secret, only the key it goes under.
func TestStripEnvRefs_RemovesValuesKeepsKeys(t *testing.T) {
	services := []Service{{
		Name: "api",
		Env:  []string{"DATABASE_URL=postgres://user:s3cr3t@db/app", "LOG_LEVEL=debug"},
	}}
	refs := map[string]string{"DATABASE_URL": "DATABASE_URL"}

	got := StripEnvRefs(services, refs)

	joined := strings.Join(got[0].Env, " ")
	if strings.Contains(joined, "s3cr3t") {
		t.Fatalf("secret survived the strip: %q", joined)
	}
	if !strings.Contains(joined, "DATABASE_URL=") {
		t.Fatalf("key was dropped instead of emptied: %q", joined)
	}
	if !strings.Contains(joined, "LOG_LEVEL=debug") {
		t.Fatalf("literal value must be preserved: %q", joined)
	}
	// The caller's slice is still used by the live supervisor: it must be intact.
	if !strings.Contains(strings.Join(services[0].Env, " "), "s3cr3t") {
		t.Fatal("StripEnvRefs mutated the caller's slice")
	}
}

func TestResolveEnvRefs_RestoresFromHostEnv(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://user:fromhost@db/app")
	stripped := []Service{{
		Name: "api",
		Env:  []string{"DATABASE_URL=", "LOG_LEVEL=debug"},
	}}
	refs := map[string]string{"DATABASE_URL": "DATABASE_URL"}

	got := ResolveEnvRefs(stripped, refs)

	joined := strings.Join(got[0].Env, " ")
	if !strings.Contains(joined, "fromhost") {
		t.Fatalf("value not restored from the host env: %q", joined)
	}
	if !strings.Contains(joined, "LOG_LEVEL=debug") {
		t.Fatalf("literal value must survive: %q", joined)
	}
}

// TestEnvRefs_RoundTrip pins the whole contract: what is stripped for the disk
// comes back identical at boot, so a reboot restarts with the same config.
func TestEnvRefs_RoundTrip(t *testing.T) {
	const secret = "postgres://user:s3cr3t@db/app"
	t.Setenv("DATABASE_URL", secret)
	original := []Service{{Name: "api", Env: []string{"DATABASE_URL=" + secret, "LOG_LEVEL=debug"}}}
	refs := map[string]string{"DATABASE_URL": "DATABASE_URL"}

	back := ResolveEnvRefs(StripEnvRefs(original, refs), refs)

	if back[0].Env[0] != "DATABASE_URL="+secret {
		t.Fatalf("round trip lost the value: %q", back[0].Env[0])
	}
}

// TestResolveEnvRefs_UnsetVarStaysEmpty guards the failure mode: a var missing
// on the host must leave the entry empty (the service fails loudly) rather than
// silently inheriting whatever else is in the environment.
func TestResolveEnvRefs_UnsetVarStaysEmpty(t *testing.T) {
	os.Unsetenv("DEFINITELY_UNSET_VAR")
	stripped := []Service{{Name: "api", Env: []string{"DATABASE_URL="}}}
	refs := map[string]string{"DATABASE_URL": "DEFINITELY_UNSET_VAR"}

	got := ResolveEnvRefs(stripped, refs)

	if got[0].Env[0] != "DATABASE_URL=" {
		t.Fatalf("unset var must leave the entry empty, got %q", got[0].Env[0])
	}
}

func TestLiteralSecretKeys_FlagsInlinedSensitiveValues(t *testing.T) {
	t.Setenv("API_TOKEN", "tok")
	cfg, err := Parse([]byte(`
version: 1
kind: deploy
name: app
services:
  api:
    image: example.com/api:1
    env:
      LLM_CREDENTIAL_ENCRYPTION_KEY: "5Kg0S1ds"
      API_TOKEN: ${API_TOKEN}
      LOG_LEVEL: "debug"
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := cfg.LiteralSecretKeys()
	if len(got) != 1 || got[0] != "LLM_CREDENTIAL_ENCRYPTION_KEY" {
		t.Fatalf("LiteralSecretKeys() = %v, want [LLM_CREDENTIAL_ENCRYPTION_KEY]", got)
	}
}
