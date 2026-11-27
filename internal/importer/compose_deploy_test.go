package importer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/internal/repofacts"
	"github.com/lalternativefabrique/sklp/spacemanager"
)

func importDeployFixture(t *testing.T, name string) *ComposeImport {
	t.Helper()
	root := filepath.Join("testdata", name)
	runnables, err := repofacts.ScanRunnable(root)
	if err != nil {
		t.Fatalf("scan runnables: %v", err)
	}
	res, err := ImportComposeDeploy(ComposeOptions{Root: root, Stack: "prod", Runnables: runnables})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res == nil {
		t.Fatal("no compose file found")
	}
	return res
}

func everyVarSet(name string) (string, bool) {
	if name == "BACKUP_S3_URL" {
		return "https://s3.example.com/backups/app/", true
	}
	return "set-" + strings.ToLower(name), true
}

func TestImportedDeployMatchesGoldenAndParses(t *testing.T) {
	for _, name := range []string{"fullstack", "unsupported", "infra-only", "prod"} {
		t.Run(name, func(t *testing.T) {
			res := importDeployFixture(t, name)
			golden := filepath.Join("testdata", name+".deploy.yaml")
			if *update {
				if err := os.WriteFile(golden, res.YAML, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("read golden (run with -update): %v", err)
			}
			if string(want) != string(res.YAML) {
				t.Errorf("deploy.yaml drifted from %s (run with -update and review the diff):\n%s", golden, res.YAML)
			}
			if _, err := spacemanager.ParseWith(res.YAML, everyVarSet); err != nil {
				t.Fatalf("sklp deploy rejects the imported file: %v\n%s", err, res.YAML)
			}
		})
	}
}
