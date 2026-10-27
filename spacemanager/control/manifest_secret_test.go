package control

import (
	"os"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/spacemanager"
)

// TestPersistManifest_NoSecretOnDisk is the end-to-end guarantee: after an
// apply, the bytes on disk must not contain the secret value, and a boot-time
// LoadManifest must hand the value back.
func TestPersistManifest_NoSecretOnDisk(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const secret = "postgres://user:sup3rs3cr3t@db/app"
	t.Setenv("DATABASE_URL", secret)

	spec := applySpec{
		Services: []spacemanager.Service{{
			Name: "api",
			Env:  []string{"DATABASE_URL=" + secret, "LOG_LEVEL=debug"},
		}},
		Meta: spacemanager.SpaceMeta{
			Name:    "production",
			EnvRefs: map[string]string{"DATABASE_URL": "DATABASE_URL"},
		},
	}
	if err := persistManifest(spec); err != nil {
		t.Fatalf("persistManifest: %v", err)
	}

	raw, err := os.ReadFile(ManifestPath("production"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if strings.Contains(string(raw), "sup3rs3cr3t") {
		t.Fatalf("secret written to disk:\n%s", raw)
	}
	if !strings.Contains(string(raw), "DATABASE_URL") {
		t.Fatalf("key missing from manifest:\n%s", raw)
	}

	// The caller's in-memory spec must keep its resolved values.
	if !strings.Contains(strings.Join(spec.Services[0].Env, " "), "sup3rs3cr3t") {
		t.Fatal("persistManifest mutated the live spec")
	}

	// Boot path: the value comes back from the host env.
	services, _, err := LoadManifest("production")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if !strings.Contains(strings.Join(services[0].Env, " "), secret) {
		t.Fatalf("value not restored at boot: %v", services[0].Env)
	}
}
