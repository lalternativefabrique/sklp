package spacemanager

import (
	"strings"
	"testing"
)

// The shape that broke gate in production: four services declare DSN,
// two of them against a different database. A single flat entry keyed by the
// env KEY kept only one template, so kratos-migrate ran Kratos' migrations
// against Hydra's database — it found the networks table Hydra had created
// and failed with "relation networks already exists", which gated kratos and
// cascaded into web and hydra rolling back for two hours.
const conflictingDeploy = `
version: 1
kind: deploy
name: gate
services:
  kratos:
    image: example.com/kratos:1
    env:
      DSN: postgres://u:${PW}@postgres:5432/kratos?sslmode=disable
  hydra:
    image: example.com/hydra:1
    env:
      DSN: postgres://u:${PW}@postgres:5432/hydra?sslmode=disable
jobs:
  kratos-migrate:
    image: example.com/kratos:1
    env:
      DSN: postgres://u:${PW}@postgres:5432/kratos?sslmode=disable
  hydra-migrate:
    image: example.com/hydra:1
    env:
      DSN: postgres://u:${PW}@postgres:5432/hydra?sslmode=disable
`

func TestEnvVarRefs_ScopesAKeyTwoServicesDeclareDifferently(t *testing.T) {
	refs, err := envVarRefs([]byte(conflictingDeploy))
	if err != nil {
		t.Fatalf("envVarRefs: %v", err)
	}

	for _, svc := range []string{"kratos", "kratos-migrate"} {
		ref, ok := LookupEnvRef(refs, svc, "DSN")
		if !ok {
			t.Fatalf("%s has no DSN ref at all", svc)
		}
		if !strings.Contains(ref, "/kratos?") {
			t.Errorf("%s resolves DSN to %q, want the kratos database", svc, ref)
		}
	}
	for _, svc := range []string{"hydra", "hydra-migrate"} {
		ref, ok := LookupEnvRef(refs, svc, "DSN")
		if !ok {
			t.Fatalf("%s has no DSN ref at all", svc)
		}
		if !strings.Contains(ref, "/hydra?") {
			t.Errorf("%s resolves DSN to %q, want the hydra database", svc, ref)
		}
	}
}

// A key every service writes the same way, or writes only once, must keep its
// bare name: scoping it would change the on-disk manifest shape for spaces
// that never had a conflict, and every one of them is already deployed.
func TestEnvVarRefs_LeavesAnUncontestedKeyUnscoped(t *testing.T) {
	refs, err := envVarRefs([]byte(`
version: 1
kind: deploy
name: app
services:
  api:
    image: example.com/api:1
    env:
      TOKEN: ${API_TOKEN}
      SHARED: postgres://u:${PW}@db/one
  worker:
    image: example.com/worker:1
    env:
      SHARED: postgres://u:${PW}@db/one
`))
	if err != nil {
		t.Fatalf("envVarRefs: %v", err)
	}
	if refs["TOKEN"] != "${API_TOKEN}" {
		t.Errorf("a key declared once was not kept bare: %v", refs)
	}
	if refs["SHARED"] != "postgres://u:${PW}@db/one" {
		t.Errorf("a key declared identically twice was not kept bare: %v", refs)
	}
	for key := range refs {
		if strings.Contains(key, envKeyScopeSep) {
			t.Errorf("nothing conflicts here, yet %q was scoped", key)
		}
	}
}

// End to end on the conflicting shape: what the manifest strips must come
// back per service, each with its own database, after the persist/restart
// round trip.
func TestEnvRefs_ConflictingKeyRoundTripsPerService(t *testing.T) {
	lookup := func(name string) (string, bool) {
		v, ok := map[string]string{"PW": "s3cr3t"}[name]
		return v, ok
	}
	cfg, err := ParseWith([]byte(conflictingDeploy), lookup)
	if err != nil {
		t.Fatal(err)
	}
	services, meta, err := cfg.Resolve("production", "")
	if err != nil {
		t.Fatal(err)
	}

	stripped := StripEnvRefs(services, meta.EnvRefs)
	for _, svc := range stripped {
		if joined := strings.Join(svc.Env, " "); strings.Contains(joined, "s3cr3t") {
			t.Fatalf("%s kept the password in the manifest: %q", svc.Name, joined)
		}
	}

	back := ResolveEnvRefsWith(stripped, meta.EnvRefs, lookup)
	want := map[string]string{
		"kratos":         "/kratos?",
		"kratos-migrate": "/kratos?",
		"hydra":          "/hydra?",
		"hydra-migrate":  "/hydra?",
	}
	for _, svc := range back {
		db, checked := want[svc.Name]
		if !checked {
			continue
		}
		joined := strings.Join(svc.Env, " ")
		if !strings.Contains(joined, db) {
			t.Errorf("%s came back pointing elsewhere: %q, want %s", svc.Name, joined, db)
		}
		if !strings.Contains(joined, "s3cr3t") {
			t.Errorf("%s did not get its password back: %q", svc.Name, joined)
		}
	}
}

// A manifest persisted before scoping carries bare keys only. Resolving it
// must still work, or every already-deployed space would lose its secrets on
// the first agent restart after the upgrade.
func TestLookupEnvRef_ReadsAPreScopingManifest(t *testing.T) {
	flat := map[string]string{"DSN": "postgres://u:${PW}@db/app"}
	ref, ok := LookupEnvRef(flat, "kratos", "DSN")
	if !ok || ref != "postgres://u:${PW}@db/app" {
		t.Fatalf("bare key not found for a scoped lookup: %q %v", ref, ok)
	}
}

// Go iterates a map in random order, so a first cut of this fix — writing
// entries as services were visited — left whichever service came first
// holding the bare key. Both halves resolved correctly, but the manifest's
// SHAPE changed between two parses of the same file, which is a spurious
// diff on every apply and the same class of non-determinism as the bug being
// fixed. The decision is made in a second pass for exactly this reason.
func TestEnvVarRefs_IsStableAcrossParses(t *testing.T) {
	first, err := envVarRefs([]byte(conflictingDeploy))
	if err != nil {
		t.Fatalf("envVarRefs: %v", err)
	}
	for i := range 50 {
		again, err := envVarRefs([]byte(conflictingDeploy))
		if err != nil {
			t.Fatalf("envVarRefs: %v", err)
		}
		if len(again) != len(first) {
			t.Fatalf("parse %d produced %d entries, first produced %d", i, len(again), len(first))
		}
		for key, want := range first {
			if again[key] != want {
				t.Fatalf("parse %d: refs[%q] = %q, first parse had %q", i, key, again[key], want)
			}
		}
	}
}
