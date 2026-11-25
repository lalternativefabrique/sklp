package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImportComposeNeverClobbersAnExistingDevYAML(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "compose.yaml"), []byte("services:\n  db:\n    image: postgres:16\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dev := filepath.Join(repo, ".sklp", "stack", "dev.yaml")
	if err := os.MkdirAll(filepath.Dir(dev), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dev, []byte("hand-written\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	imp, err := importCompose(repo, "stack", false)
	if err != nil {
		t.Fatal(err)
	}
	if imp.rel != filepath.Join(".sklp", "stack", "dev.yaml.draft") {
		t.Fatalf("import landed at %s, want the .draft next to the existing file", imp.rel)
	}
	if b, _ := os.ReadFile(dev); string(b) != "hand-written\n" {
		t.Fatalf("existing dev.yaml was overwritten: %q", b)
	}
	draft, _ := os.ReadFile(filepath.Join(repo, imp.rel))
	if !strings.Contains(string(draft), "image: postgres:16") {
		t.Fatalf("draft lacks the imported service:\n%s", draft)
	}

	if _, err := importCompose(repo, "stack", true); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dev); !strings.Contains(string(b), "image: postgres:16") {
		t.Fatalf("-force must write dev.yaml itself, got:\n%s", b)
	}
}

func TestImportComposeWithoutComposeFileIsANoop(t *testing.T) {
	imp, err := importCompose(t.TempDir(), "stack", false)
	if err != nil || imp != nil {
		t.Fatalf("want nil, nil; got %v, %v", imp, err)
	}
}
