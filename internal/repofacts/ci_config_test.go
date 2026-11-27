package repofacts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"
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

func TestCIConfig_SharedModuleNamesStayDistinctScopes(t *testing.T) {
	f := &RepoFacts{Languages: []LanguageFacts{
		{Lang: "python", Modules: []ModuleFacts{{Path: "a/backend"}, {Path: "b/backend"}, {Path: "full"}}},
		{Lang: "node", PackageManager: "pnpm", Modules: []ModuleFacts{{Path: "full"}}},
	}}
	out, _ := CIConfig(f)
	var doc struct {
		Scopes map[string][]string `yaml:"scopes"`
		Steps  []struct {
			Name  string   `yaml:"name"`
			Scope []string `yaml:"scope"`
		} `yaml:"steps"`
	}
	if err := yamlv3.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("draft is not valid YAML: %v\n%s", err, out)
	}
	if len(doc.Scopes) != 3 || doc.Scopes["a-backend"] == nil || doc.Scopes["b-backend"] == nil || doc.Scopes["full"] == nil {
		t.Fatalf("scopes = %v", doc.Scopes)
	}
	names := map[string]bool{}
	for _, s := range doc.Steps {
		if names[s.Name] {
			t.Fatalf("step %q repeated:\n%s", s.Name, out)
		}
		names[s.Name] = true
	}
	if !names["test-full-python"] || !names["test-full-node"] || !names["test-a-backend"] {
		t.Fatalf("steps = %v", names)
	}
}

func TestScanSkipsTmp(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"go.mod", "tmp/bench/go.mod"} {
		_ = os.MkdirAll(filepath.Join(root, filepath.Dir(p)), 0o755)
		_ = os.WriteFile(filepath.Join(root, p), []byte("module x\n\ngo 1.26\n"), 0o644)
	}
	files, err := walkRepo(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != filepath.Join(root, "go.mod") {
		t.Fatalf("walked %v", files)
	}
}
