package importer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lalternativefabrique/sklp/spacemanager"
	"gopkg.in/yaml.v3"
)

var k8sExamples = filepath.Join("..", "..", "examples", "import", "k8s")

// TestK8sExamples imports every public manifest set under
// examples/import/k8s and checks the result against its committed
// deploy.yaml, then against the deploy parser.
func TestK8sExamples(t *testing.T) {
	cases, err := filepath.Glob(filepath.Join(k8sExamples, "*", "case.yaml"))
	if err != nil || len(cases) == 0 {
		t.Fatalf("no example under %s: %v", k8sExamples, err)
	}
	for _, file := range cases {
		dir := filepath.Dir(file)
		t.Run(filepath.Base(dir), func(t *testing.T) {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var c struct {
				Path string `yaml:"path"`
			}
			if err := yaml.Unmarshal(raw, &c); err != nil {
				t.Fatal(err)
			}
			source := filepath.Join("source", c.Path)
			if kustomizationFile(filepath.Join(dir, source)) != "" {
				if _, err := KustomizeCommand("."); err != nil {
					t.Skip(err)
				}
			}
			res, err := ImportK8sDeploy(K8sOptions{Root: dir, Stack: "prod", Sources: []string{source}})
			if err != nil {
				t.Fatalf("import: %v", err)
			}
			golden := filepath.Join(dir, "deploy.yaml")
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
				t.Errorf("deploy.yaml drifted from %s (run with -update and review the diff)", golden)
			}
			if _, err := spacemanager.ParseWith(res.YAML, everyVarSet); err != nil {
				t.Fatalf("sklp deploy rejects the imported file: %v", err)
			}
		})
	}
}
