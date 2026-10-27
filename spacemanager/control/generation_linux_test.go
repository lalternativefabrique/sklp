package control

import (
	"os"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/spacemanager"
)

func envOf(svc spacemanager.Service) map[string]string {
	out := map[string]string{}
	for _, e := range svc.Env {
		k, v, _ := strings.Cut(e, "=")
		out[k] = v
	}
	return out
}

// applyGeneration persists a generation the way applyDesired does: snapshot,
// env, then manifest.
func applyGeneration(t *testing.T, spec applySpec) {
	t.Helper()
	if err := snapshotGeneration(spec.Meta.Name); err != nil {
		t.Fatal(err)
	}
	if err := persistSpaceEnv(spec.Meta.Name, resolvedSecrets(spec), spec.Meta.EnvValues != nil); err != nil {
		t.Fatal(err)
	}
	if err := persistManifest(spec); err != nil {
		t.Fatal(err)
	}
}

// TestRepersistLive_KeptServiceUsesItsOwnGeneration replays the shop
// incident: a full apply renames an env key together with the image, core
// crashes and is rolled back while web moves. The manifest must persist core
// with the previous generation's refs and web with the current one, no secret
// in clear, and both must resolve at boot.
func TestRepersistLive_KeptServiceUsesItsOwnGeneration(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const space = "production"

	gen1 := applySpec{
		Services: []spacemanager.Service{
			{Name: "core", Image: "rg/core:1", DeclaredImage: "rg/core:1", Env: []string{"SHOP_MASTER_KEY=old-secret", "PORT=4100"}},
			{Name: "web", Image: "rg/web:1", DeclaredImage: "rg/web:1", Env: []string{"JWT_SECRET=jwt-old"}},
		},
		Meta: spacemanager.SpaceMeta{
			Name:      space,
			EnvRefs:   map[string]string{"SHOP_MASTER_KEY": "${SHOP_MASTER_KEY}", "JWT_SECRET": "${JWT_SECRET}"},
			EnvValues: map[string]string{"SHOP_MASTER_KEY": "old-secret", "JWT_SECRET": "jwt-old"},
		},
	}
	applyGeneration(t, gen1)

	gen2 := applySpec{
		Services: []spacemanager.Service{
			{Name: "core", Image: "rg/core:2", DeclaredImage: "rg/core:2", Env: []string{"PARTAGG_MASTER_KEY=new-secret", "PORT=4100"}},
			{Name: "web", Image: "rg/web:2", DeclaredImage: "rg/web:2", Env: []string{"JWT_SECRET=jwt-new"}},
		},
		Meta: spacemanager.SpaceMeta{
			Name:      space,
			EnvRefs:   map[string]string{"PARTAGG_MASTER_KEY": "${PARTAGG_MASTER_KEY}", "JWT_SECRET": "${JWT_SECRET}"},
			EnvValues: map[string]string{"PARTAGG_MASTER_KEY": "new-secret", "JWT_SECRET": "jwt-new"},
		},
	}
	applyGeneration(t, gen2)

	live := []spacemanager.Service{
		{Name: "core", Image: "rg/core:1", Env: []string{"SHOP_MASTER_KEY=old-secret", "PORT=4100"}},
		{Name: "web", Image: "rg/web:2", Env: []string{"JWT_SECRET=jwt-new"}},
	}
	if err := RepersistLive(space, live); err != nil {
		t.Fatalf("RepersistLive: %v", err)
	}

	raw, err := os.ReadFile(ManifestPath(space))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"old-secret", "new-secret", "jwt-old", "jwt-new"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("secret %q written in clear:\n%s", secret, raw)
		}
	}

	services, meta, err := ReadPersistedManifest(space)
	if err != nil {
		t.Fatal(err)
	}
	core, web := services[0], services[1]
	if core.Image != "rg/core:1" || core.DeclaredImage != "rg/core:2" {
		t.Fatalf("core = image %q declared %q, want the kept image and the declared one", core.Image, core.DeclaredImage)
	}
	if _, ok := envOf(core)["SHOP_MASTER_KEY"]; !ok {
		t.Fatalf("core lost its own env key: %v", core.Env)
	}
	if web.Image != "rg/web:2" || web.DeclaredImage != "rg/web:2" {
		t.Fatalf("web = image %q declared %q", web.Image, web.DeclaredImage)
	}
	if meta.EnvRefs["SHOP_MASTER_KEY"] != "${SHOP_MASTER_KEY}" || meta.EnvRefs["PARTAGG_MASTER_KEY"] != "${PARTAGG_MASTER_KEY}" {
		t.Fatalf("EnvRefs must carry both generations' keys: %v", meta.EnvRefs)
	}

	booted, _, err := LoadManifest(space)
	if err != nil {
		t.Fatal(err)
	}
	if got := envOf(booted[0])["SHOP_MASTER_KEY"]; got != "old-secret" {
		t.Fatalf("kept core resolves SHOP_MASTER_KEY=%q at boot, want the previous value", got)
	}
	if got := envOf(booted[1])["JWT_SECRET"]; got != "jwt-new" {
		t.Fatalf("moved web resolves JWT_SECRET=%q at boot, want the current value", got)
	}
}

// TestRepersistLive_UnknownGenerationStripsEveryRef: a live spec matching
// neither generation still never lands with a referenced value in clear.
func TestRepersistLive_UnknownGenerationStripsEveryRef(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const space = "production"
	applyGeneration(t, applySpec{
		Services: []spacemanager.Service{{Name: "core", Image: "rg/core:1", Env: []string{"OLD_KEY=old-secret"}}},
		Meta:     spacemanager.SpaceMeta{Name: space, EnvRefs: map[string]string{"OLD_KEY": "${OLD_KEY}"}, EnvValues: map[string]string{"OLD_KEY": "old-secret"}},
	})
	applyGeneration(t, applySpec{
		Services: []spacemanager.Service{{Name: "core", Image: "rg/core:2", Env: []string{"NEW_KEY=new-secret"}}},
		Meta:     spacemanager.SpaceMeta{Name: space, EnvRefs: map[string]string{"NEW_KEY": "${NEW_KEY}"}, EnvValues: map[string]string{"NEW_KEY": "new-secret"}},
	})

	live := []spacemanager.Service{{Name: "core", Image: "rg/core:0", Env: []string{"OLD_KEY=old-secret", "NEW_KEY=new-secret"}}}
	if err := RepersistLive(space, live); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(ManifestPath(space))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "-secret") {
		t.Fatalf("referenced value written in clear:\n%s", raw)
	}
}

// TestRepersistLive_NoPreviousGeneration: the first apply ever has nothing to
// snapshot; a rollback then persists what serves, stripped with the current
// refs, exactly as before.
func TestRepersistLive_NoPreviousGeneration(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const space = "production"
	applyGeneration(t, applySpec{
		Services: []spacemanager.Service{{Name: "core", Image: "rg/core:1", Env: []string{"KEY=secret"}}},
		Meta:     spacemanager.SpaceMeta{Name: space, EnvRefs: map[string]string{"KEY": "${KEY}"}, EnvValues: map[string]string{"KEY": "secret"}},
	})
	if _, err := os.Stat(PrevManifestPath(space)); !os.IsNotExist(err) {
		t.Fatalf("first apply must leave no previous generation, stat err = %v", err)
	}
	if err := RepersistLive(space, []spacemanager.Service{{Name: "core", Image: "rg/core:1", Env: []string{"KEY=secret"}}}); err != nil {
		t.Fatal(err)
	}
	services, _, err := LoadManifest(space)
	if err != nil {
		t.Fatal(err)
	}
	if got := envOf(services[0])["KEY"]; got != "secret" {
		t.Fatalf("KEY=%q after repersist, want resolved at boot", got)
	}
}
