package pipeline

import (
	"testing"

	yaml "go.yaml.in/yaml/v4"
)

// YAML 1.1 folds a bare `on` key to the boolean true (the "Norway problem").
// If the loader does that, every `on:` block in the repo is silently dropped.
func TestBareOnKeyIsReadAsTheOnField(t *testing.T) {
	var p Pipeline
	if err := yaml.Unmarshal([]byte("version: 1\nkind: pipeline\nname: x\non:\n  except: [main]\n"), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.On == nil {
		t.Fatal("on: block was dropped — the bare key did not bind to the On field")
	}
	if len(p.On.Except) != 1 || p.On.Except[0] != "main" {
		t.Fatalf("On.Except = %v, want [main]", p.On.Except)
	}
}
