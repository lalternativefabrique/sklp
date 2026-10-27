package devstack

import (
	"testing"

	"github.com/lalternativefabrique/sklp/pipeline/runnerimage"
)

// TestToolchainSpecs_CarriesBuildTags pins that `sklp dev` compiles go_tools
// with the tags space.yaml declares. It used to drop them — the third return
// of GoToolSktSpec was discarded as "image-only" — which built `migrate`
// without its postgres driver, so every dev stack came up against a database
// no migration had ever touched.
func TestToolchainSpecs_CarriesBuildTags(t *testing.T) {
	got := toolchainSpecs(runnerimage.Runner{
		Toolchain: map[string]string{"go": "1.26.6"},
		GoTools: []string{
			"github.com/golang-migrate/migrate/v4/cmd/migrate@v4.19.1 +tags=postgres",
			"gotest.tools/gotestsum@v1.13.0",
		},
	})

	byName := map[string]devTool{}
	for _, tool := range got {
		byName[tool.spec] = tool
	}

	migrate, ok := byName["migrate@4.19.1"]
	if !ok {
		t.Fatalf("migrate spec missing from %v", got)
	}
	if migrate.tags != "postgres" {
		t.Errorf("migrate tags = %q, want postgres", migrate.tags)
	}

	if tags := byName["gotestsum@1.13.0"].tags; tags != "" {
		t.Errorf("gotestsum tags = %q, want none", tags)
	}
	if tags := byName["go@1.26.6"].tags; tags != "" {
		t.Errorf("language toolchain carries tags = %q, want none", tags)
	}
}
