//go:build linux

package control

import (
	"github.com/lalternativefabrique/sklp/spacemanager"
	"os"
	"strings"
	"testing"
)

// The secrets a remote apply resolved must come back after a restart, or the
// space returns half-running: a private image cannot be pulled and a fresh
// Postgres cannot initialise.
func TestSpaceEnv_RoundTripsWithoutTouchingTheProcess(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("SCW_SECRET_KEY", "")

	if err := WriteSpaceEnv("production", map[string]string{
		"SCW_SECRET_KEY":    "registry-secret",
		"POSTGRES_PASSWORD": "db-secret",
	}); err != nil {
		t.Fatalf("WriteSpaceEnv: %v", err)
	}
	vars, err := ReadSpaceEnv("production")
	if err != nil {
		t.Fatalf("ReadSpaceEnv: %v", err)
	}
	if vars["SCW_SECRET_KEY"] != "registry-secret" || vars["POSTGRES_PASSWORD"] != "db-secret" {
		t.Errorf("vars = %v, want the persisted values", vars)
	}
	if got := os.Getenv("SCW_SECRET_KEY"); got != "" {
		t.Errorf("SCW_SECRET_KEY leaked into the process environment: %q", got)
	}
}

// The file holds secrets, so it must never be readable beyond its owner.
func TestSpaceEnv_IsOwnerOnly(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if err := WriteSpaceEnv("production", map[string]string{"K": "v"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(SpaceEnvPath("production"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
}

// A value the host already provides wins: an operator's dotenv, or the local
// deploy path, is authoritative over what a past apply happened to resolve.
func TestSpaceEnv_EachSpaceKeepsItsOwnValue(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("PG_S3_ACCESS_KEY", "from-the-host")

	refs := map[string]string{"PGBACKREST_REPO1_S3_KEY": "PG_S3_ACCESS_KEY"}
	spaces := []struct{ space, key string }{{"shop-production", "shop-key"}, {"shop-production", "shop-key"}}
	for _, sp := range spaces {
		svc := []spacemanager.Service{{Name: "postgres", Image: "pg:17", Env: []string{"PGBACKREST_REPO1_S3_KEY="}}}
		if err := persistManifest(applySpec{Services: svc, Meta: spacemanager.SpaceMeta{Name: sp.space, EnvRefs: refs}}); err != nil {
			t.Fatal(err)
		}
		if err := WriteSpaceEnv(sp.space, map[string]string{"PG_S3_ACCESS_KEY": sp.key}); err != nil {
			t.Fatal(err)
		}
	}
	for _, sp := range spaces {
		services, _, err := LoadManifest(sp.space)
		if err != nil {
			t.Fatalf("LoadManifest(%s): %v", sp.space, err)
		}
		if got := services[0].Env[0]; got != "PGBACKREST_REPO1_S3_KEY="+sp.key {
			t.Errorf("%s resolved %q, want its own %q (not the host's, not another space's)", sp.space, got, sp.key)
		}
	}
}

// A re-apply that resolves nothing must not leave the previous secrets behind
// for the unit to keep sourcing.
func TestSpaceEnv_EmptyRemovesTheFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if err := WriteSpaceEnv("production", map[string]string{"K": "v"}); err != nil {
		t.Fatal(err)
	}
	if err := WriteSpaceEnv("production", nil); err != nil {
		t.Fatalf("WriteSpaceEnv(nil): %v", err)
	}
	if _, err := os.Stat(SpaceEnvPath("production")); !os.IsNotExist(err) {
		t.Error("the env file survived an apply that resolved no secret")
	}
}

// A newline would inject a second variable into the dotenv, quietly meaning
// something else than what was applied.
func TestSpaceEnv_RefusesAValueThatCannotRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	err := WriteSpaceEnv("production", map[string]string{"K": "line1\nEVIL=1"})
	if err == nil {
		t.Fatal("a value carrying a newline was accepted")
	}
	if !strings.Contains(err.Error(), "K") {
		t.Errorf("the error does not name the offending key: %v", err)
	}
}
