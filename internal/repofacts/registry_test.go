package repofacts

import "testing"

func TestRegistry_FromSklpConfig(t *testing.T) {
	dir := mkRepo(t, map[string]string{
		".sklp/config.yaml": "runner:\n  registry: rg.fr-par.scw.cloud/acme\n  registry_auth_secret: x\n",
	})
	f, err := Scan(dir)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if f.Registry != "rg.fr-par.scw.cloud/acme" {
		t.Errorf("registry = %q, want from .sklp/config.yaml", f.Registry)
	}
}

func TestRegistry_SklpConfigPlaceholderIgnored(t *testing.T) {
	// The placeholder `sklp init -only config` writes when it can't infer
	// a registry must NOT be promoted to a real value.
	dir := mkRepo(t, map[string]string{
		".sklp/config.yaml": "runner:\n  registry: rg.fr-par.scw.cloud/<your-namespace>\n",
	})
	f, err := Scan(dir)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if f.Registry != "" {
		t.Errorf("placeholder registry should be ignored, got %q", f.Registry)
	}
}

func TestRegistry_SklpConfigBeatsMakefile(t *testing.T) {
	// .sklp/config.yaml is the operator's explicit declaration and wins
	// over a Makefile REGISTRY assignment.
	dir := mkRepo(t, map[string]string{
		".sklp/config.yaml": "runner:\n  registry: ghcr.io/from-config\n",
		"Makefile":          "REGISTRY ?= ghcr.io/from-makefile\n",
	})
	f, err := Scan(dir)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if f.Registry != "ghcr.io/from-config" {
		t.Errorf("registry = %q, want .sklp/config.yaml to win", f.Registry)
	}
}

func TestRegistry_FallsBackToMakefileWhenNoConfig(t *testing.T) {
	dir := mkRepo(t, map[string]string{
		"Makefile": "REGISTRY := ghcr.io/from-makefile\n",
	})
	f, err := Scan(dir)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if f.Registry != "ghcr.io/from-makefile" {
		t.Errorf("registry = %q, want makefile fallback", f.Registry)
	}
}
