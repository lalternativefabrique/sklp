package repofacts

import (
	"os"
	"testing"
)

// osWriteFile is a tiny test helper to keep individual test bodies
// short. Mode 0644 matches the runtime default for generated files.
func osWriteFile(path string, b []byte) error {
	return os.WriteFile(path, b, 0o644)
}

func TestSuggestDevServices_GoApiAndPnpmWeb(t *testing.T) {
	// Mirrors a monorepo layout: one Go module under services/api, one
	// pnpm workspace under apps/web. Both already pre-classified as
	// IsApp=true to bypass the runnable scanner (tested separately).
	runnables := []RunnableFacts{
		{
			Lang:       "go",
			Module:     ModuleFacts{Path: "services/api", Name: "github.com/x/skalpai/services/api", Manifest: "go.mod"},
			IsApp:      true,
			Reason:     "has main package at module root",
			Entrypoint: "go run .",
		},
		{
			Lang:           "node",
			PackageManager: "pnpm",
			Module:         ModuleFacts{Path: "apps/web", Name: "@skalpai/web", Manifest: "package.json"},
			IsApp:          true,
			Reason:         "package.json scripts.dev present",
			Entrypoint:     "pnpm --filter @skalpai/web dev",
		},
	}
	got := SuggestDevServices(runnables)
	if len(got) != 2 {
		t.Fatalf("want 2 services, got %d (%+v)", len(got), got)
	}
	// Find by name
	byName := map[string]SuggestedService{}
	for _, s := range got {
		byName[s.Name] = s
	}
	api, ok := byName["api"]
	if !ok {
		t.Fatalf("missing api service: %+v", got)
	}
	if api.Run != "go run ." || api.Workdir != "services/api" {
		t.Errorf("unexpected api: %+v", api)
	}
	web, ok := byName["web"]
	if !ok {
		t.Fatalf("missing web service: %+v", got)
	}
	if web.Run != "pnpm --filter @skalpai/web dev" {
		t.Errorf("unexpected web run: %q", web.Run)
	}
	if len(web.DependsOn) != 1 || web.DependsOn[0] != "api" {
		t.Errorf("web should depend on api, got %v", web.DependsOn)
	}
}

func TestSuggestDevServices_EmptyRepo(t *testing.T) {
	if got := SuggestDevServices(nil); got != nil {
		t.Errorf("empty repo should return nil, got %+v", got)
	}
}

func TestSuggestInfraServices_DevboxDetected(t *testing.T) {
	dir := t.TempDir()
	if err := writeTestFile(dir+"/devbox.json", "{}"); err != nil {
		t.Fatal(err)
	}
	got := SuggestInfraServices(dir)
	if len(got) != 1 {
		t.Fatalf("want 1 infra service, got %d", len(got))
	}
	if got[0].Name != "devbox-infra" || got[0].Optional {
		t.Errorf("unexpected: %+v", got[0])
	}
	if got[0].Run != "devbox services up" {
		t.Errorf("unexpected run: %q", got[0].Run)
	}
}

func TestSuggestInfraServices_ProcfileDetected(t *testing.T) {
	dir := t.TempDir()
	if err := writeTestFile(dir+"/Procfile", "web: bundle exec puma\n"); err != nil {
		t.Fatal(err)
	}
	got := SuggestInfraServices(dir)
	if len(got) != 1 {
		t.Fatalf("want 1 infra service, got %d", len(got))
	}
	if got[0].Name != "procfile-infra" {
		t.Errorf("unexpected name: %s", got[0].Name)
	}
}

func TestSuggestInfraServices_NoneFound(t *testing.T) {
	if got := SuggestInfraServices(t.TempDir()); len(got) != 0 {
		t.Errorf("expected no infra, got %+v", got)
	}
}

func TestSuggestInfraServices_MultipleConventionsCoexist(t *testing.T) {
	// A repo mid-migration may carry both — we emit both as
	// optional services so the user picks.
	dir := t.TempDir()
	_ = writeTestFile(dir+"/devbox.json", "{}")
	_ = writeTestFile(dir+"/Tiltfile", "")
	got := SuggestInfraServices(dir)
	if len(got) != 2 {
		t.Fatalf("want 2 infra services, got %d (%+v)", len(got), got)
	}
}

func writeTestFile(path, content string) error {
	return osWriteFile(path, []byte(content))
}

func TestSuggestDevServices_LibrariesAreFiltered(t *testing.T) {
	// IsApp=false entries (libraries detected by the runnable scanner)
	// must not produce services even though they're passed in.
	runnables := []RunnableFacts{
		{Lang: "node", Module: ModuleFacts{Path: "packages/sdk", Manifest: "package.json"}, IsApp: false, Reason: "library"},
		{Lang: "go", Module: ModuleFacts{Path: "packages/sdk-go", Manifest: "go.mod"}, IsApp: false, Reason: "library"},
	}
	if got := SuggestDevServices(runnables); len(got) != 0 {
		t.Errorf("expected no services for libs, got %+v", got)
	}
}
