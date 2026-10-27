package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lalternativefabrique/sklp/spacemanager"
)

func TestGatewayDriverFromConfigMissingFileDefaults(t *testing.T) {
	dir := t.TempDir()
	got, err := gatewayDriverFromConfig("deploy.yaml", dir)
	if err != nil {
		t.Fatalf("gatewayDriverFromConfig (missing): %v", err)
	}
	if got != spacemanager.DefaultGatewayDriver {
		t.Errorf("driver = %q, want default %q", got, spacemanager.DefaultGatewayDriver)
	}
}

func TestGatewayDriverFromConfigReadsDriver(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deploy.yaml")
	if err := os.WriteFile(path, []byte("gateway:\n  driver: traefik\nservices:\n  core:\n    image: img\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := gatewayDriverFromConfig("deploy.yaml", dir)
	if err != nil {
		t.Fatalf("gatewayDriverFromConfig: %v", err)
	}
	if got != "traefik" {
		t.Errorf("driver = %q, want traefik", got)
	}
}
