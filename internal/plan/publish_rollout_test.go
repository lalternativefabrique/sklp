package plan

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/lalternativefabrique/sklp/internal/rolloutgate"
)

func rolloutGraph(t *testing.T) *rolloutgate.Graph {
	t.Helper()
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".sklp"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		".sklp/deploy-root.yaml":   "deployments:\n  - space: app\n    path: .sklp/deploy.yaml\n",
		".sklp/image-watches.yaml": "watches:\n  - image_repo: reg/migrate\n",
		".sklp/deploy.yaml":        "services:\n  core:\n    image: reg/core:1\n    depends_on: [migrate]\njobs:\n  migrate:\n    image: reg/migrate:1\n",
	}
	for path, body := range files {
		if err := os.WriteFile(filepath.Join(repo, path), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	g, err := rolloutgate.Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestOrderPublishStepsPutsDependencyFirst(t *testing.T) {
	steps := []CIStepInput{
		{Name: "lint"},
		{Name: "core", Kind: string(KindCIStepPublish), Image: "reg/core"},
		{Name: "migrate", Kind: string(KindCIStepPublish), Image: "reg/migrate"},
	}
	var got []string
	for _, s := range orderPublishSteps(steps, rolloutGraph(t)) {
		got = append(got, s.Name)
	}
	if want := []string{"lint", "migrate", "core"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestMarkRolloutWaits(t *testing.T) {
	g := rolloutGraph(t)
	migrate := &PublishSpec{Image: "reg/migrate"}
	core := &PublishSpec{Image: "reg/core"}
	markRolloutWaits([]CIStep{{Publish: migrate}, {Publish: core}}, g)
	if !migrate.WaitRollout || core.WaitRollout {
		t.Fatalf("migrate=%v core=%v", migrate.WaitRollout, core.WaitRollout)
	}
}

func TestMigrateAloneDoesNotWait(t *testing.T) {
	migrate := &PublishSpec{Image: "reg/migrate"}
	markRolloutWaits([]CIStep{{Publish: migrate}, {Status: StatusSkippedCompile}}, rolloutGraph(t))
	if migrate.WaitRollout {
		t.Fatal("nothing ships after migrate, so publish must not wait")
	}
}
