package control

import (
	"testing"

	"github.com/lalternativefabrique/sklp/spacemanager"
)

// seedFullManifest persists a manifest exercising every field the read route
// reports, through the SAME persist path an apply takes (so the env stripping
// under test is the real one, not a fixture).
func seedFullManifest(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	services := []spacemanager.Service{
		{
			Name:      "core",
			Image:     "rg.example.com/core:1",
			Replicas:  2,
			Port:      4100,
			Volumes:   []string{"data:/var/lib"},
			DependsOn: []string{"migrate"},
			Env: []string{
				"DATABASE_URL=postgres://real-secret",
				"POSTGRES_DB=skalpel",
				"API_TOKEN=inlined-literal",
				"DSN=postgres://app:real-secret@postgres:5432/app",
			},
			Resources: spacemanager.ServiceResources{CPUCores: 2, MemoryBytes: 2 << 30},
			Ready:     &spacemanager.Probe{HTTP: "http://localhost:4100/health", Timeout: "90s"},
		},
		{Name: "migrate", Image: "rg.example.com/migrate:1", Oneshot: true},
		{Name: "ship-dumps", Image: "amazon/aws-cli", Schedule: "20 3 * * *"},
	}
	meta := spacemanager.SpaceMeta{
		Name:  "production",
		Stack: "skalpai",
		EnvRefs: map[string]string{
			"DATABASE_URL": "DATABASE_URL",
			"DSN":          "postgres://app:${POSTGRES_PASSWORD}@postgres:5432/app",
		},
		EnvValues: map[string]string{"POSTGRES_PASSWORD": "real-secret"},
		Routes:    []spacemanager.ResolvedRoute{{Service: "core", Host: "api.example.com"}},
	}
	if err := persistManifest(applySpec{Services: services, Meta: meta}); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}
}

// The route reports the declared shape, and splits long-running services from
// the jobs (oneshot or scheduled) that a service list would otherwise mix in.
func TestBuildManifestReportsDeclaredShape(t *testing.T) {
	seedFullManifest(t)

	got, err := buildManifest("production")
	if err != nil {
		t.Fatalf("build manifest: %v", err)
	}
	if got.Space != "production" || got.Stack != "skalpai" {
		t.Fatalf("meta = %+v", got)
	}
	if len(got.Services) != 1 || got.Services[0].Name != "core" {
		t.Fatalf("services = %+v", got.Services)
	}
	if len(got.Jobs) != 2 {
		t.Fatalf("jobs = %+v, want migrate + ship-dumps", got.Jobs)
	}
	core := got.Services[0]
	if core.Replicas != 2 || core.Port != 4100 {
		t.Fatalf("core = %+v", core)
	}
	if core.Resources == nil || core.Resources.CPUCores != 2 {
		t.Fatalf("resources = %+v", core.Resources)
	}
	if core.Ready == nil || core.Ready.HTTP == "" {
		t.Fatalf("ready = %+v", core.Ready)
	}
	if len(got.Routes) != 1 || got.Routes[0].Host != "api.example.com" {
		t.Fatalf("routes = %+v", got.Routes)
	}
}

// The security contract of this route: a ${VAR} env is reported by name only.
// The value is set in THIS process's environment, which is exactly the state a
// live supervisor is in — LoadManifest would resolve it back and hand it out,
// which is why the read path must not use LoadManifest.
func TestBuildManifestNeverServesResolvedSecrets(t *testing.T) {
	seedFullManifest(t)
	t.Setenv("DATABASE_URL", "postgres://real-secret")

	got, err := buildManifest("production")
	if err != nil {
		t.Fatalf("build manifest: %v", err)
	}

	var seen int
	for _, entry := range got.Services[0].Env {
		switch entry.Key {
		case "DATABASE_URL":
			seen++
			if entry.Source != EnvSourceReference {
				t.Fatalf("DATABASE_URL source = %q, want %q", entry.Source, EnvSourceReference)
			}
			if entry.Value != "" {
				t.Fatalf("DATABASE_URL leaked its value: %q", entry.Value)
			}
			if entry.Ref != "DATABASE_URL" {
				t.Fatalf("DATABASE_URL ref = %q", entry.Ref)
			}
		case "POSTGRES_DB":
			seen++
			if entry.Source != EnvSourceLiteral || entry.Value != "skalpel" {
				t.Fatalf("POSTGRES_DB = %+v", entry)
			}
		case "API_TOKEN":
			seen++
			if entry.Source != EnvSourceSensitive {
				t.Fatalf("API_TOKEN source = %q, want %q so the UI masks it", entry.Source, EnvSourceSensitive)
			}
		case "DSN":
			seen++
			if entry.Source != EnvSourceReference || entry.Ref != "POSTGRES_PASSWORD" {
				t.Fatalf("DSN = %+v, want a reference to POSTGRES_PASSWORD", entry)
			}
			if entry.Value != "postgres://app:${POSTGRES_PASSWORD}@postgres:5432/app" {
				t.Fatalf("DSN value = %q, want the template with its placeholder", entry.Value)
			}
		}
	}
	if seen != 4 {
		t.Fatalf("expected all four env entries, saw %d", seen)
	}
}

// A space that was never deployed here has no manifest: the caller gets an
// error to turn into a 404, not an empty manifest that would read as "declares
// nothing".
func TestBuildManifestUnknownSpace(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if _, err := buildManifest("never-deployed"); err == nil {
		t.Fatal("want an error for a space with no persisted manifest")
	}
}
