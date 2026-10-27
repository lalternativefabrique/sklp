package repofacts

import (
	"strings"
	"testing"
)

func TestCIConfig_GoModule(t *testing.T) {
	f := &RepoFacts{
		Languages: []LanguageFacts{{
			Lang:             "go",
			ToolchainVersion: "1.25",
			Modules:          []ModuleFacts{{Path: "apps/api", Name: "api", Manifest: "go.mod"}},
		}},
	}
	yaml, _ := CIConfig(f)
	for _, want := range []string{
		"kind: pipeline",
		`go: "1.25"`,
		"scopes:",
		"api:",
		`- "apps/api/**"`,
		"name: test-api",
		"scope: [api]",
		"cd apps/api && go test ./...",
	} {
		if !strings.Contains(yaml, want) {
			t.Errorf("ci.yaml missing %q:\n%s", want, yaml)
		}
	}
}

func TestCIConfig_Deterministic(t *testing.T) {
	f := &RepoFacts{Languages: []LanguageFacts{
		{Lang: "node", PackageManager: "pnpm", PackageManagerVersion: "9.0.0",
			Modules: []ModuleFacts{{Path: "web", Manifest: "package.json"}}},
		{Lang: "go", ToolchainVersion: "1.25",
			Modules: []ModuleFacts{{Path: "api", Manifest: "go.mod"}}},
	}}
	a, _ := CIConfig(f)
	b, _ := CIConfig(f)
	if a != b {
		t.Errorf("CIConfig not deterministic:\n%s\n---\n%s", a, b)
	}
	// Scopes are emitted in sorted order: api before web.
	if strings.Index(a, "api:") > strings.Index(a, "web:") {
		t.Errorf("scopes should be sorted (api before web):\n%s", a)
	}
}

func TestCIConfig_NodePnpm(t *testing.T) {
	f := &RepoFacts{Languages: []LanguageFacts{{
		Lang: "node", PackageManager: "pnpm", PackageManagerVersion: "9.0.0",
		Modules: []ModuleFacts{{Path: "web", Manifest: "package.json"}},
	}}}
	yaml, _ := CIConfig(f)
	if !strings.Contains(yaml, "cd web && pnpm run test") {
		t.Errorf("expected pnpm test command:\n%s", yaml)
	}
	if !strings.Contains(yaml, `pnpm: "9.0.0"`) {
		t.Errorf("expected pinned pnpm in tools:\n%s", yaml)
	}
}

func TestCIConfig_NoModules(t *testing.T) {
	yaml, notes := CIConfig(&RepoFacts{})
	if !strings.Contains(yaml, "kind: pipeline") {
		t.Errorf("should still emit a valid skeleton:\n%s", yaml)
	}
	if len(notes) == 0 {
		t.Error("expected a NOTE when no modules were found")
	}
}
