package devstack

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestDeclaredPortsListsEveryBoundPort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dev.yaml")
	yaml := `version: 1
kind: devstack
name: dev
services:
  gen:
    run: make gen
    oneshot: true
  web:
    run: pnpm dev
    port: 5280
  api:
    run: go run .
    port: 4100
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	got := declaredPorts(path)
	slices.Sort(got)
	if !slices.Equal(got, []int{4100, 5280}) {
		t.Fatalf("declaredPorts = %v", got)
	}
	if got := declaredPorts(filepath.Join(t.TempDir(), "missing.yaml")); got != nil {
		t.Fatalf("missing file: %v", got)
	}
}
