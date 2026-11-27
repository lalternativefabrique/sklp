package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/internal/config"
)

func TestAnExplicitContextThatNamesNothingIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("SKLP_CONFIG", path)
	cfg := &config.Config{}
	cfg.SetContext("demo", config.Context{Server: "https://demo.example:6464"})
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}

	if err := (&netFlags{}).explicitContextErr(); err != nil {
		t.Fatalf("no -context keeps today's behaviour: %v", err)
	}
	if err := (&netFlags{context: "demo"}).explicitContextErr(); err != nil {
		t.Fatalf("a known context: %v", err)
	}
	err := (&netFlags{context: "typo"}).explicitContextErr()
	if err == nil || !strings.Contains(err.Error(), "sklp deploy login -context typo") {
		t.Fatalf("an unknown context must say how to connect: %v", err)
	}
	if _, derr := (&netFlags{context: "typo"}).dialClient("production"); derr == nil {
		t.Fatal("dialClient must not fall back to the local node on an unknown context")
	}
}
