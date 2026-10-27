package pipelineselect

import (
	"os"
	"path/filepath"
	"testing"
)

// writePipeline drops a minimal pipeline YAML into dir.
func writePipeline(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func names(sel []SelectedPipeline) []string {
	out := make([]string, len(sel))
	for i, s := range sel {
		out[i] = s.Name
	}
	return out
}

func TestSelectPipelines(t *testing.T) {
	dir := t.TempDir()

	writePipeline(t, dir, "ci-core", `version: 1
kind: pipeline
name: ci-core
scopes:
  core: ["services/api/**"]
steps:
  - name: test
    run: go test ./...
`)
	writePipeline(t, dir, "ci-web", `version: 1
kind: pipeline
name: ci-web
scopes:
  web: ["apps/web/**"]
steps:
  - name: test
    run: pnpm test
`)
	// scopes-empty pipeline → must never be auto-selected.
	writePipeline(t, dir, "publish", `version: 1
kind: pipeline
name: publish
steps:
  - name: push
    run: echo push
`)
	// template file → ignored by the glob.
	writePipeline(t, dir, "_base", `version: 1
kind: pipeline
scopes:
  core: ["services/api/**"]
steps:
  - name: x
    run: echo x
`)

	cases := []struct {
		name    string
		changed []string
		want    []string
	}{
		{"single match", []string{"services/api/main.go"}, []string{"ci-core"}},
		{"no match", []string{"docs/readme.md"}, nil},
		{"multiple match sorted", []string{"apps/web/app.tsx", "services/api/x.go"}, []string{"ci-core", "ci-web"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sel, err := SelectPipelines(dir, "feat/x", c.changed)
			if err != nil {
				t.Fatalf("SelectPipelines: %v", err)
			}
			got := names(sel)
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("got %v, want %v", got, c.want)
				}
			}
		})
	}
}

func TestSelectPipelinesEmptyScopesSkipped(t *testing.T) {
	dir := t.TempDir()
	writePipeline(t, dir, "publish", `version: 1
kind: pipeline
name: publish
steps:
  - name: push
    run: echo push
`)
	sel, err := SelectPipelines(dir, "feat/x", []string{"anything"})
	if err != nil {
		t.Fatalf("SelectPipelines: %v", err)
	}
	if len(sel) != 0 {
		t.Fatalf("scopes-empty pipeline must be skipped, got %v", names(sel))
	}
}

// A scoped pipeline pinned to main via require.branches must NOT be selected on
// a feature branch (it would only be refused at require-time), but must be
// selected when the current branch is main.
func TestSelectPipelinesRequireBranchesFilter(t *testing.T) {
	dir := t.TempDir()
	writePipeline(t, dir, "secops", `version: 1
kind: pipeline
name: secops
require:
  branches: [main]
scopes:
  core: ["services/api/**"]
steps:
  - name: scan
    run: echo scan
`)
	changed := []string{"services/api/main.go"}

	onFeature, err := SelectPipelines(dir, "feat/x", changed)
	if err != nil {
		t.Fatalf("SelectPipelines(feat): %v", err)
	}
	if len(onFeature) != 0 {
		t.Fatalf("main-only pipeline must be skipped on feature branch, got %v", names(onFeature))
	}

	onMain, err := SelectPipelines(dir, "main", changed)
	if err != nil {
		t.Fatalf("SelectPipelines(main): %v", err)
	}
	if len(onMain) != 1 || onMain[0].Name != "secops" {
		t.Fatalf("main-only pipeline must be selected on main, got %v", names(onMain))
	}
}

// A merge re-runs what already ran on the pull request. A validation pipeline
// says so itself rather than the webhook assuming it for every repo.
func TestSelectPipelinesOnExceptSkipsThatBranch(t *testing.T) {
	dir := t.TempDir()
	writePipeline(t, dir, "ci", `version: 1
kind: pipeline
name: ci
on:
  except: [main]
scopes:
  core: ["services/api/**"]
steps:
  - name: test
    run: echo test
`)
	changed := []string{"services/api/main.go"}

	onFeature, err := SelectPipelines(dir, "feat/x", changed)
	if err != nil {
		t.Fatalf("SelectPipelines(feat): %v", err)
	}
	if len(onFeature) != 1 || onFeature[0].Name != "ci" {
		t.Fatalf("got %v, want [ci] — except must not narrow anything but the branches it lists", names(onFeature))
	}

	onMain, err := SelectPipelines(dir, "main", changed)
	if err != nil {
		t.Fatalf("SelectPipelines(main): %v", err)
	}
	if len(onMain) != 0 {
		t.Fatalf("got %v, want none on the excepted branch", names(onMain))
	}
}

// publish declares scopes (they pick which images to build) AND
// require.branches [main] (it may only run there). Reading that permission as
// an intent selected it on main — the same branch where a merge triggers it by
// name, so every merge published twice. `on: never` is how it opts out.
func TestSelectPipelinesOnNeverIsSelectedNowhere(t *testing.T) {
	dir := t.TempDir()
	writePipeline(t, dir, "publish", `version: 1
kind: pipeline
name: publish
on:
  never: true
require:
  branches: [main]
scopes:
  core: ["services/api/**"]
steps:
  - name: image
    run: echo build
`)
	changed := []string{"services/api/main.go"}

	for _, branch := range []string{"main", "feat/x", ""} {
		got, err := SelectPipelines(dir, branch, changed)
		if err != nil {
			t.Fatalf("SelectPipelines(%q): %v", branch, err)
		}
		if len(got) != 0 {
			t.Fatalf("branch %q: got %v, want none — a publish pipeline is started by name only", branch, names(got))
		}
	}
}

// Omitting the block keeps the previous behaviour, so pipelines that never
// declare `on:` are unaffected by its introduction.
func TestSelectPipelinesWithoutOnBlockIsUnrestricted(t *testing.T) {
	dir := t.TempDir()
	writePipeline(t, dir, "ci", `version: 1
kind: pipeline
name: ci
scopes:
  core: ["services/api/**"]
steps:
  - name: test
    run: echo test
`)
	changed := []string{"services/api/main.go"}

	for _, branch := range []string{"main", "feat/x"} {
		got, err := SelectPipelines(dir, branch, changed)
		if err != nil {
			t.Fatalf("SelectPipelines(%q): %v", branch, err)
		}
		if len(got) != 1 {
			t.Fatalf("branch %q: got %v, want [ci]", branch, names(got))
		}
	}
}
