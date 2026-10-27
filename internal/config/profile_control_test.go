package config

import (
	"testing"
)

// TestContextRoundTripsControlFields: server + ca_fingerprint + space + token
// all survive a Save/Load round-trip on a deploy context.
func TestContextRoundTripsControlFields(t *testing.T) {
	withXDG(t)
	cfg := &Config{
		API:            API{URL: "https://api.skalpel.com", Token: "sklp_pat_def_uvw"},
		CurrentContext: "prod",
		Contexts: map[string]Context{
			"prod": {
				Server:        "https://vm.example:9443",
				CAFingerprint: "abc123def456",
				Space:         "production",
				Token:         "node-bearer",
			},
		},
	}
	path, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, _, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	c := got.Contexts["prod"]
	if c.Server != "https://vm.example:9443" {
		t.Fatalf("server = %q", c.Server)
	}
	if c.CAFingerprint != "abc123def456" {
		t.Fatalf("ca_fingerprint = %q", c.CAFingerprint)
	}
	if c.Space != "production" || c.Token != "node-bearer" {
		t.Fatalf("context fields not preserved: %+v", c)
	}
	// The api block is stored independently of the context.
	if got.API.URL != "https://api.skalpel.com" || got.API.Token != "sklp_pat_def_uvw" {
		t.Fatalf("api block not preserved: %+v", got.API)
	}
}

// TestContextBackwardCompatNoControlFields: a legacy config written without
// any deploy transport loads fine, feeds the api block, and creates NO
// context — so nothing is selectable as a deploy target.
func TestContextBackwardCompatNoControlFields(t *testing.T) {
	path := withXDG(t)
	writeConfigFile(t, path, ""+
		"current: default\n"+
		"profiles:\n"+
		"  default:\n"+
		"    api_url: http://localhost:4100\n"+
		"    token: sklp_pat_abc\n")

	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.API.URL != "http://localhost:4100" || cfg.API.Token != "sklp_pat_abc" {
		t.Fatalf("api block wrong: %+v", cfg.API)
	}
	if len(cfg.Contexts) != 0 {
		t.Fatalf("contexts should be empty: %+v", cfg.Contexts)
	}
	if cfg.CurrentContext != "" {
		t.Fatalf("CurrentContext should be empty, got %q", cfg.CurrentContext)
	}
}
