package plan

import (
	"path/filepath"
	"strings"
	"testing"
)

// The registries file holds push credentials. It is part of no sandbox grant,
// so a `run:` step a pipeline declares cannot read it — unlike config.yaml,
// which IS granted read-only for context resolution. A future grant reaching
// the sklp config dir wholesale would expose it silently; this test is what
// catches that.
func TestSandboxNeverGrantsRegistriesFile(t *testing.T) {
	home := t.TempDir()
	sklpDir := filepath.Join(home, ".config", "sklp")
	spec := sandboxSpec{
		Repo:            t.TempDir(),
		Cmdline:         "docker build .",
		SklpUserConfig:  filepath.Join(sklpDir, "config.yaml"),
		DockerConfigDir: filepath.Join(home, ".docker"),
	}
	for _, a := range computeFSPolicy(spec, spec.Repo) {
		for _, p := range a.Paths {
			if strings.Contains(p, "registries.yaml") {
				t.Fatalf("sandbox granted the registries file: %s (%s)", p, a.Kind)
			}
			if filepath.Clean(p) == filepath.Clean(sklpDir) {
				t.Fatalf("sandbox granted the sklp config dir, which contains registries.yaml: %s (%s)", p, a.Kind)
			}
		}
	}
}
