package layout

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// mkTree builds a fake .sklp/ tree: map[dir] -> []files.
func mkTree(t *testing.T, tree map[string][]string) string {
	t.Helper()
	repo := t.TempDir()
	for dir, files := range tree {
		full := filepath.Join(repo, ".sklp", dir)
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if err := os.WriteFile(filepath.Join(full, f), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return repo
}

func sampleRepo(t *testing.T) string {
	return mkTree(t, map[string][]string{
		"core":       {"dev.yaml", "deploy.yaml", "space.yaml"},
		"transcript": {"deploy.yaml", "space.yaml"}, // deploy-only (no dev)
		"empty":      {},                            // stack dir, no contracts
		"pipelines":  {"ci.yaml", "publish.yaml"},
		"tasks":      {"legacy.yaml"},
	})
}

func TestStacksExcludesPipelinesAndTasks(t *testing.T) {
	l := New(sampleRepo(t))
	got, err := l.Stacks()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"core", "empty", "transcript"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Stacks() = %v, want %v", got, want)
	}
}

func TestStacksForDev(t *testing.T) {
	l := New(sampleRepo(t))
	got, err := l.StacksFor("dev")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"core"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("StacksFor(dev) = %v, want %v", got, want)
	}
	gotDeploy, _ := l.StacksFor("deploy")
	wantDeploy := []string{"core", "transcript"}
	if !reflect.DeepEqual(gotDeploy, wantDeploy) {
		t.Fatalf("StacksFor(deploy) = %v, want %v", gotDeploy, wantDeploy)
	}
}

func TestResolveOK(t *testing.T) {
	repo := sampleRepo(t)
	l := New(repo)
	got, err := l.Resolve("core", "deploy")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("Resolve path not absolute: %s", got)
	}
	if !strings.HasSuffix(got, filepath.Join(".sklp", "core", "deploy.yaml")) {
		t.Fatalf("Resolve = %s, unexpected suffix", got)
	}
}

func TestResolveStackNotFound(t *testing.T) {
	l := New(sampleRepo(t))
	_, err := l.Resolve("ghost", "dev")
	if !errors.Is(err, ErrStackNotFound) {
		t.Fatalf("want ErrStackNotFound, got %v", err)
	}
}

func TestResolveNoContractListsVerbs(t *testing.T) {
	l := New(sampleRepo(t))
	_, err := l.Resolve("transcript", "dev")
	if !errors.Is(err, ErrNoContract) {
		t.Fatalf("want ErrNoContract, got %v", err)
	}
	for _, v := range []string{"deploy", "space"} {
		if !strings.Contains(err.Error(), v) {
			t.Fatalf("error %q should list verb %q", err.Error(), v)
		}
	}
}

func TestVerbs(t *testing.T) {
	l := New(sampleRepo(t))
	got, err := l.Verbs("core")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"deploy", "dev", "space"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Verbs(core) = %v, want %v", got, want)
	}
	if _, err := l.Verbs("ghost"); !errors.Is(err, ErrStackNotFound) {
		t.Fatalf("Verbs(ghost) want ErrStackNotFound, got %v", err)
	}
}

func TestPipelinesExcludesTemplatesCanonicalWins(t *testing.T) {
	repo := mkTree(t, map[string][]string{
		"pipelines": {"ci.yaml", "publish.yml", "_base.yaml"},
		"tasks":     {"legacy.yaml"},
	})
	l := New(repo)
	got, err := l.Pipelines()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ci", "publish"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Pipelines() = %v, want %v", got, want)
	}
	if !strings.HasSuffix(l.PipelinesDir(), "pipelines") {
		t.Fatalf("PipelinesDir should prefer canonical, got %s", l.PipelinesDir())
	}
}

func TestPipelinesLegacyFallback(t *testing.T) {
	repo := mkTree(t, map[string][]string{
		"tasks": {"ci.yaml", "_tpl.yaml"},
	})
	l := New(repo)
	got, err := l.Pipelines()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ci"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Pipelines() = %v, want %v", got, want)
	}
	if !strings.HasSuffix(l.PipelinesDir(), "tasks") {
		t.Fatalf("PipelinesDir should fall back to tasks, got %s", l.PipelinesDir())
	}
}
