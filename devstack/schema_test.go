package devstack

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lalternativefabrique/sklp/pipeline"
)

func TestTopoSort_Linear(t *testing.T) {
	svcs := map[string]pipeline.Service{
		"web": {Run: "x", DependsOn: []string{"api"}},
		"api": {Run: "x"},
	}
	got, err := topoSort(svcs)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "api" || got[1] != "web" {
		t.Fatalf("want [api web], got %v", got)
	}
}

func TestTopoSort_AlphaTieBreak(t *testing.T) {
	// No deps among any of the three — order must be alphabetical so
	// log diffs across runs stay stable.
	svcs := map[string]pipeline.Service{
		"web":  {Run: "x"},
		"api":  {Run: "x"},
		"otel": {Run: "x"},
	}
	got, _ := topoSort(svcs)
	want := []string{"api", "otel", "web"}
	for i, n := range want {
		if got[i] != n {
			t.Fatalf("want %v, got %v", want, got)
		}
	}
}

func TestTopoSort_RejectsCycle(t *testing.T) {
	svcs := map[string]pipeline.Service{
		"a": {Run: "x", DependsOn: []string{"b"}},
		"b": {Run: "x", DependsOn: []string{"a"}},
	}
	if _, err := topoSort(svcs); err == nil {
		t.Fatal("expected cycle error")
	}
}

func TestTopoSort_RejectsUnknownDep(t *testing.T) {
	svcs := map[string]pipeline.Service{
		"a": {Run: "x", DependsOn: []string{"ghost"}},
	}
	if _, err := topoSort(svcs); err == nil {
		t.Fatal("expected unknown-dep error")
	}
}

func TestLoad_ParsesDevstack(t *testing.T) {
	dir := t.TempDir()
	yaml := []byte(`version: 1
kind: devstack
name: dev
env_files: [.env]
services:
  api:
    run: go run .
    workdir: services/api
    ready:
      http: http://localhost:4100/health
      timeout: 10s
  web:
    run: pnpm dev
    depends_on: [api]
`)
	path := filepath.Join(dir, "dev.yaml")
	if err := writeFile(path, yaml); err != nil {
		t.Fatal(err)
	}
	st, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Services) != 2 {
		t.Fatalf("want 2 services, got %d", len(st.Services))
	}
	if st.Services[0].Name != "api" || st.Services[1].Name != "web" {
		t.Fatalf("want [api web], got [%s %s]", st.Services[0].Name, st.Services[1].Name)
	}
	if st.Services[0].ReadyTimeout.Seconds() != 10 {
		t.Fatalf("want 10s ready timeout, got %v", st.Services[0].ReadyTimeout)
	}
}

func TestLoad_AutoInjectsLogProbeWhenDependedOn(t *testing.T) {
	dir := t.TempDir()
	yaml := []byte(`version: 1
kind: devstack
services:
  infra: { run: "echo up" }
  core:
    run: "echo core"
    depends_on: [infra]
`)
	path := filepath.Join(dir, "dev.yaml")
	if err := writeFile(path, yaml); err != nil {
		t.Fatal(err)
	}
	st, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, ns := range st.Services {
		if ns.Name != "infra" {
			continue
		}
		if ns.Def.Ready == nil {
			t.Fatal("expected Ready to be auto-injected on infra")
		}
		if ns.Def.Ready.Log == "" {
			t.Errorf("expected Log probe, got %+v", ns.Def.Ready)
		}
	}
}

func TestLoad_OneshotWithReadyIsRejected(t *testing.T) {
	dir := t.TempDir()
	yaml := []byte(`version: 1
kind: devstack
services:
  migrate:
    run: "echo migrate"
    oneshot: true
    ready:
      http: http://localhost:1
`)
	path := filepath.Join(dir, "dev.yaml")
	_ = writeFile(path, yaml)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for oneshot+ready combination")
	}
}

func TestLoad_RejectsRunAndImageTogether(t *testing.T) {
	dir := t.TempDir()
	yaml := []byte(`version: 1
kind: devstack
services:
  bad:
    run: "echo a"
    image: "postgres:16-alpine"
`)
	path := filepath.Join(dir, "dev.yaml")
	_ = writeFile(path, yaml)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for run+image combination")
	}
}

func TestLoad_RejectsImageOnlyFieldsOnRunService(t *testing.T) {
	dir := t.TempDir()
	// volumes/command stay image-only on a run: service.
	yaml := []byte(`version: 1
kind: devstack
services:
  bad:
    run: "echo a"
    volumes: ["data:/data"]
`)
	path := filepath.Join(dir, "dev.yaml")
	_ = writeFile(path, yaml)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error: volumes on a run: service")
	}
}

// TestLoad_AcceptsPortsOnRunService: ports: ARE allowed on a run: service so a
// dev server (vite, a go http server) running as a container in the embedded
// space can publish its port to the host browser via the ingress.
func TestLoad_AcceptsPortsOnRunService(t *testing.T) {
	dir := t.TempDir()
	yaml := []byte(`version: 1
kind: devstack
services:
  web:
    run: "vite dev"
    ports: ["3000:3000"]
`)
	path := filepath.Join(dir, "dev.yaml")
	_ = writeFile(path, yaml)
	if _, err := Load(path); err != nil {
		t.Fatalf("ports on a run: service must be accepted, got: %v", err)
	}
}

func TestLoad_AcceptsImageService(t *testing.T) {
	dir := t.TempDir()
	yaml := []byte(`version: 1
kind: devstack
services:
  postgres:
    image: postgres:16-alpine
    ports: ["5432:5432"]
    env:
      POSTGRES_USER: app
    ready:
      tcp: localhost:5432
`)
	path := filepath.Join(dir, "dev.yaml")
	_ = writeFile(path, yaml)
	st, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Services) != 1 || st.Services[0].Def.Image != "postgres:16-alpine" {
		t.Fatalf("expected image service, got %+v", st.Services)
	}
}

func TestLoad_RejectsWrongKind(t *testing.T) {
	dir := t.TempDir()
	yaml := []byte(`version: 1
kind: pipeline
services:
  api: { run: x }
`)
	path := filepath.Join(dir, "dev.yaml")
	if err := writeFile(path, yaml); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected kind error")
	}
}

func writeFile(path string, b []byte) error {
	return os.WriteFile(path, b, 0o644)
}
