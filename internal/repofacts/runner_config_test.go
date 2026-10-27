package repofacts

import (
	"strings"
	"testing"
)

func TestRunnerConfig_PinsPackageManagerVersion(t *testing.T) {
	f := &RepoFacts{Languages: []LanguageFacts{
		{Lang: "node", ToolchainVersion: "20.11.0", PackageManager: "pnpm", PackageManagerVersion: "8.15.0"},
	}}
	yaml, _ := RunnerConfig(f)
	if !strings.Contains(yaml, `pnpm: "8.15.0"`) {
		t.Errorf("expected pinned pnpm version, got:\n%s", yaml)
	}
}

func TestRunnerConfig_NodeLtsFallbackWhenPMButNoNodeVersion(t *testing.T) {
	// pnpm detected but no node version → must still emit node (lts),
	// else the config bakes pnpm with no node to run it on.
	f := &RepoFacts{Languages: []LanguageFacts{
		{Lang: "node", PackageManager: "pnpm"},
	}}
	yaml, _ := RunnerConfig(f)
	if !strings.Contains(yaml, `node: "lts"`) {
		t.Errorf("expected node: lts fallback, got:\n%s", yaml)
	}
	if !strings.Contains(yaml, `pnpm: "latest"`) {
		t.Errorf("expected pnpm: latest, got:\n%s", yaml)
	}
}

func TestRunnerConfig_MapsToolchainAndRegistry(t *testing.T) {
	f := &RepoFacts{
		Registry: "rg.fr-par.scw.cloud/acme",
		Languages: []LanguageFacts{
			{Lang: "go", ToolchainVersion: "1.22"},
			{Lang: "node", ToolchainVersion: "v20.11.0", PackageManager: "pnpm"},
		},
	}
	yaml, _ := RunnerConfig(f)
	for _, want := range []string{
		"registry: rg.fr-par.scw.cloud/acme",
		`go: "1.22"`,
		`node: "20.11.0"`, // v-prefix stripped
		`pnpm: "latest"`,
		"GITHUB_TOKEN", // go present
	} {
		if !strings.Contains(yaml, want) {
			t.Errorf("missing %q in:\n%s", want, yaml)
		}
	}
}

func TestRunnerConfig_Deterministic(t *testing.T) {
	// Map iteration in the toolchain must be sorted — the config hash
	// keys the runner image, so two runs must be byte-identical.
	f := &RepoFacts{
		Languages: []LanguageFacts{
			{Lang: "go", ToolchainVersion: "1.25.0"},
			{Lang: "node", ToolchainVersion: "20", PackageManager: "pnpm"},
			{Lang: "python", ToolchainVersion: "3.12"},
		},
	}
	first, _ := RunnerConfig(f)
	for i := 0; i < 30; i++ {
		got, _ := RunnerConfig(f)
		if got != first {
			t.Fatalf("non-deterministic output at iter %d:\n--- first ---\n%s\n--- got ---\n%s", i, first, got)
		}
	}
}

func TestRunnerConfig_NoRegistryAddsNote(t *testing.T) {
	f := &RepoFacts{Languages: []LanguageFacts{{Lang: "go", ToolchainVersion: "1.22"}}}
	yaml, notes := RunnerConfig(f)
	if !strings.Contains(yaml, "<your-namespace>") {
		t.Error("expected registry placeholder")
	}
	var found bool
	for _, n := range notes {
		if strings.Contains(n, "registry") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a registry NOTE, got %v", notes)
	}
}

func TestRunnerConfig_GoTools(t *testing.T) {
	f := &RepoFacts{
		Languages:    []LanguageFacts{{Lang: "go", ToolchainVersion: "1.22"}},
		Makefile:     &MakefileFacts{Path: "Makefile", Targets: []string{"lint", "test"}},
		APIContracts: APIContracts{Tools: APIContractTools{Swag: true}},
	}
	yaml, _ := RunnerConfig(f)
	for _, want := range []string{"golangci-lint", "swag"} {
		if !strings.Contains(yaml, want) {
			t.Errorf("expected go_tool %q in:\n%s", want, yaml)
		}
	}
}

func TestRunnerConfig_NoGoToolsWithoutGo(t *testing.T) {
	f := &RepoFacts{Languages: []LanguageFacts{{Lang: "node", ToolchainVersion: "20", PackageManager: "pnpm"}}}
	yaml, _ := RunnerConfig(f)
	if strings.Contains(yaml, "go_tools") {
		t.Errorf("a node-only project must not get go_tools:\n%s", yaml)
	}
	if strings.Contains(yaml, "GITHUB_TOKEN") {
		t.Errorf("a node-only project should not auto-add GITHUB_TOKEN:\n%s", yaml)
	}
}
