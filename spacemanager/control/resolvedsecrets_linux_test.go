//go:build linux

package control

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/spacemanager"
)

// The values collected are the ones the manifest is about to drop: they must be
// read from the spec, not the environment — on a remote apply the supervisor's
// own env is empty of them, which is why the space could not come back.
func TestResolvedSecrets_CollectsWhatTheManifestDrops(t *testing.T) {
	spec := applySpec{
		Services: []spacemanager.Service{{
			Name: "core",
			Env: []string{
				"DATABASE_URL=postgres://app:s3cret@postgres:5432/app",
				"PORT=4100",
			},
		}},
		Meta: spacemanager.SpaceMeta{
			Name:    "production",
			EnvRefs: map[string]string{"DATABASE_URL": "DATABASE_URL"},
			Registries: map[string]spacemanager.RegistryAuthMeta{
				"rg.fr-par.scw.cloud": {PasswordEnv: "SCW_SECRET_KEY", Password: "registry-secret"},
			},
		},
	}

	got := resolvedSecrets(spec)

	if got["DATABASE_URL"] != "postgres://app:s3cret@postgres:5432/app" {
		t.Errorf("a ${VAR} service env was not collected: %v", got)
	}
	if got["SCW_SECRET_KEY"] != "registry-secret" {
		t.Errorf("the registry password was not collected: %v", got)
	}
	// PORT is a literal, not a secret: persisting it would grow the file with
	// values the manifest already keeps.
	if _, ok := got["PORT"]; ok {
		t.Error("a literal env value was persisted as a secret")
	}
}

// The map must be keyed by the ref NAME (${S3_ACCESS_KEY}), not the env key
// (PGBACKREST_REPO1_S3_KEY): ResolveEnvRefs re-resolves by name at boot, so a
// key-keyed file left every `KEY: ${OTHER_NAME}` secret empty after an agent
// restart — postgres lost its WAL archiving credentials
// while POSTGRES_PASSWORD (same-name) kept working by coincidence.
func TestResolvedSecrets_RoundTripsThroughResolveEnvRefs(t *testing.T) {
	refs := map[string]string{
		"PGBACKREST_REPO1_S3_KEY": "S3_ACCESS_KEY",
		"AWS_ACCESS_KEY_ID":       "S3_ACCESS_KEY",
		"POSTGRES_PASSWORD":       "POSTGRES_PASSWORD",
	}
	spec := applySpec{
		Services: []spacemanager.Service{{
			Name: "postgres",
			Env: []string{
				"PGBACKREST_REPO1_S3_KEY=ak-123",
				"AWS_ACCESS_KEY_ID=ak-123",
				"POSTGRES_PASSWORD=pw-456",
			},
		}},
		Meta: spacemanager.SpaceMeta{Name: "production", EnvRefs: refs},
	}

	got := resolvedSecrets(spec)
	if got["S3_ACCESS_KEY"] != "ak-123" {
		t.Fatalf("secret not keyed by its ref name: %v", got)
	}
	if _, ok := got["PGBACKREST_REPO1_S3_KEY"]; ok {
		t.Fatalf("secret keyed by env key instead of ref name: %v", got)
	}

	// Full restart path: what WriteSpaceEnv persists is what loadSpaceEnv puts
	// in the environment, and ResolveEnvRefs must find every value there.
	for name, value := range got {
		t.Setenv(name, value)
	}
	stripped := spacemanager.StripEnvRefs(spec.Services, refs)
	resolved := spacemanager.ResolveEnvRefs(stripped, refs)
	for _, entry := range resolved[0].Env {
		key, value, _ := strings.Cut(entry, "=")
		if value == "" {
			t.Errorf("env %q resolved empty after the persist/restart round-trip", key)
		}
	}
}

// A local apply ships no registry password (the supervisor shares the
// operator's env), so there is nothing to persist and no empty file to write.
func TestResolvedSecrets_EmptyOnTheLocalPath(t *testing.T) {
	spec := applySpec{
		Services: []spacemanager.Service{{Name: "core", Env: []string{"PORT=4100"}}},
		Meta: spacemanager.SpaceMeta{
			Name:       "production",
			Registries: map[string]spacemanager.RegistryAuthMeta{"host": {PasswordEnv: "SCW_SECRET_KEY"}},
		},
	}

	if got := resolvedSecrets(spec); len(got) != 0 {
		t.Errorf("resolvedSecrets = %v, want none on a local apply", got)
	}
}

// A composed template cannot be split back into its vars from the resolved
// spec: the values ride in Meta.EnvValues and land in the env file by NAME, so
// ExpandTemplate finds them at boot.
func TestResolvedSecrets_TakesTemplateVarsFromEnvValues(t *testing.T) {
	spec := applySpec{
		Services: []spacemanager.Service{{
			Name: "core",
			Env:  []string{"DSN=postgres://app:s3cret@postgres:5432/app", "PGPASSWORD=s3cret"},
		}},
		Meta: spacemanager.SpaceMeta{
			Name: "production",
			EnvRefs: map[string]string{
				"DSN":        "postgres://app:${POSTGRES_PASSWORD}@postgres:5432/app",
				"PGPASSWORD": "${POSTGRES_PASSWORD}",
			},
			EnvValues: map[string]string{"POSTGRES_PASSWORD": "s3cret"},
		},
	}

	got := resolvedSecrets(spec)

	if got["POSTGRES_PASSWORD"] != "s3cret" {
		t.Fatalf("template var not collected: %v", got)
	}
	if _, ok := got["DSN"]; ok {
		t.Fatalf("a composed value must not be persisted under its env key: %v", got)
	}
	if len(got) != 1 {
		t.Fatalf("got = %v", got)
	}
}

func TestPersistManifestDropsEnvValues(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	spec := applySpec{
		Services: []spacemanager.Service{{Name: "core", Env: []string{"DSN=postgres://app:s3cret@db/app"}}},
		Meta: spacemanager.SpaceMeta{
			Name:      "production",
			EnvRefs:   map[string]string{"DSN": "postgres://app:${PW}@db/app"},
			EnvValues: map[string]string{"PW": "s3cret"},
		},
	}
	if err := persistManifest(spec); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(ManifestPath("production"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "s3cret") {
		t.Fatalf("secret persisted in the manifest: %s", data)
	}
	if !strings.Contains(string(data), "${PW}") {
		t.Fatalf("template missing from the manifest: %s", data)
	}
}

func TestApplyFromSourceWritesTemplateVarsToSpaceEnv(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	sup := &fakeSupervisor{live: map[string]spacemanager.Service{}, differs: map[string]bool{}, routesChanged: true}
	c, cancel := serveFake(t, "app-production", sup)
	defer cancel()
	src := ApplySource{
		Stack: "prod",
		Vars:  map[string]string{"APP_HOST": "example.org", "DB_PASS": "p4ss"},
		YAML: `
services:
  web:
    image: img:1
    port: 5273
    env:
      BETTER_AUTH_URL: https://${APP_HOST}
      DATABASE_URL: postgres://app:${DB_PASS}@postgres:5432/app
`,
	}
	if _, err := c.ApplySource(context.Background(), src, false); err != nil {
		t.Fatal(err)
	}
	env, err := ReadSpaceEnv("app-production")
	if err != nil {
		t.Fatal(err)
	}
	if env["APP_HOST"] != "example.org" || env["DB_PASS"] != "p4ss" {
		t.Fatalf("space env = %v, want every template var", env)
	}
}
