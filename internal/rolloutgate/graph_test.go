package rolloutgate

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const (
	coreRepo    = "reg/app/core"
	webRepo     = "reg/app/web"
	migrateRepo = "reg/app/migrate"
)

func graph(t *testing.T) *Graph {
	t.Helper()
	repo := t.TempDir()
	write := func(path, body string) {
		full := filepath.Join(repo, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".sklp/deploy-root.yaml", "deployments:\n  - space: app\n    path: .sklp/prod/deploy.yaml\n")
	write(".sklp/image-watches.yaml", "watches:\n  - image_repo: reg/app/migrate\n")
	write(".sklp/prod/deploy.yaml", `
services:
  postgres:
    image: postgres:17
  core:
    image: reg/app/core:1
    depends_on: [migrate, postgres]
  web:
    image: reg/app/web:1
    depends_on: [core]
jobs:
  migrate:
    image: reg/app/migrate:1
    depends_on: [postgres]
`)
	g, err := Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestLoadWithoutDeployRootHasNoGraph(t *testing.T) {
	g, err := Load(t.TempDir())
	if err != nil || g != nil {
		t.Fatalf("Load = %v, %v", g, err)
	}
}

func TestOrderFollowsDependsOn(t *testing.T) {
	repos := []string{coreRepo, webRepo, migrateRepo}
	got := graph(t).Order(repos)
	if want := []int{2, 0, 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Order = %v, want %v", got, want)
	}
}

func TestBeforeIsTransitive(t *testing.T) {
	g := graph(t)
	if !g.Before(migrateRepo, webRepo) {
		t.Fatal("migrate must precede web through core")
	}
	if g.Before(webRepo, migrateRepo) {
		t.Fatal("web must not precede migrate")
	}
}

func TestWatched(t *testing.T) {
	g := graph(t)
	if !g.Watched(migrateRepo) || g.Watched(coreRepo) {
		t.Fatal("only migrate is watched")
	}
}

func TestImageRepo(t *testing.T) {
	cases := map[string]string{
		"reg:5000/a/b:1":     "reg:5000/a/b",
		"reg:5000/a/b":       "reg:5000/a/b",
		"reg/a@sha256:abc":   "reg/a",
		"postgres:17-alpine": "postgres",
	}
	for in, want := range cases {
		if got := ImageRepo(in); got != want {
			t.Errorf("ImageRepo(%q) = %q, want %q", in, got, want)
		}
	}
}
