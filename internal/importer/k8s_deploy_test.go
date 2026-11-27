package importer

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/lalternativefabrique/sklp/spacemanager"
)

func TestImportedK8sDeployMatchesGoldenAndParses(t *testing.T) {
	for _, name := range []string{"k8s-kustomize", "k8s-plain"} {
		t.Run(name, func(t *testing.T) {
			if _, err := KustomizeCommand("."); err != nil && name == "k8s-kustomize" {
				t.Skip(err)
			}
			res, err := ImportK8sDeploy(K8sOptions{Root: filepath.Join("testdata", name), Stack: "prod"})
			if err != nil {
				t.Fatalf("import: %v", err)
			}
			if res == nil {
				t.Fatal("no manifests found")
			}
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

func TestFindK8sSourcesPrefersProductionOverlays(t *testing.T) {
	got := FindK8sSources(filepath.Join("testdata", "k8s-kustomize"))
	want := []string{filepath.Join("infra", "k8s", "overlays", "production")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sources = %v, want %v", got, want)
	}
}
