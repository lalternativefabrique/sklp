package spacemanager

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/spacemanager/deployvars"
)

func fakeLookup(name string) (string, bool) {
	if strings.HasPrefix(name, "UNSET_") {
		return "", false
	}
	return strings.ReplaceAll(strings.ToLower(name), "_", "-") + ".example.org", true
}

func clientResolved(t *testing.T, raw []byte, space, stack string, lookup func(string) (string, bool)) ([]Service, SpaceMeta) {
	t.Helper()
	cfg, err := ParseWith(raw, lookup)
	if err != nil {
		t.Fatalf("client parse: %v", err)
	}
	services, meta, err := cfg.Resolve(space, "")
	if err != nil {
		t.Fatalf("client resolve: %v", err)
	}
	meta.Stack = stack
	meta.Registries = ResolveRegistryPasswordsWith(meta.Registries, lookup)
	return services, meta
}

func nodeResolved(t *testing.T, raw []byte, space, stack string, lookup func(string) (string, bool)) ([]Service, SpaceMeta) {
	t.Helper()
	vars, err := deployvars.Collect(raw, lookup)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	services, meta, err := ResolveSource(raw, vars, space, stack)
	if err != nil {
		t.Fatalf("node resolve: %v", err)
	}
	return services, meta
}

func assertSameResolution(t *testing.T, raw []byte, space, stack string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cs, cm := clientResolved(t, raw, space, stack, fakeLookup)
	ns, nm := nodeResolved(t, raw, space, stack, fakeLookup)
	want, _ := json.MarshalIndent(applySpecForTest{cs, cm}, "", " ")
	got, _ := json.MarshalIndent(applySpecForTest{ns, nm}, "", " ")
	if string(want) != string(got) {
		t.Fatalf("space %q: node resolution differs from the client one\nclient:\n%s\nnode:\n%s", space, want, got)
	}
}

type applySpecForTest struct {
	Services []Service `json:"services"`
	Meta     SpaceMeta `json:"meta"`
}

const equivalenceDeployYAML = `
# a commented reference still counts: ${COMMENTED_VAR}
env_files: [.env]

registries:
  rg.example.com:
    username: nologin
    password: ${REGISTRY_TOKEN}

gateway:
  email: ops@example.org

services:
  postgres:
    image: postgres:17-alpine
    port: 5432
    env:
      POSTGRES_PASSWORD: ${DB_PASS}
    volumes:
      - pgdata:/var/lib/postgresql/data
    backup:
      command: ["pg_dumpall"]
      schedule: "0 3 * * *"
  web:
    image: rg.example.com/app/web:1
    port: 5273
    replicas: 2
    depends_on: [postgres, migrate]
    env:
      PUBLIC_URL: https://${APP_HOST}/${SPACE}
      DATABASE_URL: postgres://app:${DB_PASS}@postgres:5432/app
      LOG_LEVEL: info
      SHARED: ${SHARED_WEB}
    ready:
      http: /health
  worker:
    image: rg.example.com/app/worker:1
    env:
      SHARED: ${SHARED_WORKER}
      API_TOKEN: ${API_TOKEN}

jobs:
  migrate:
    image: rg.example.com/app/migrate:1
    command: ["postgres://app:${DB_PASS}@postgres:5432/app", "up"]
    depends_on: [postgres]

spaces:
  app-production:
    web:
      replicas: 3
      env:
        LOG_LEVEL: warn
    gateway:
      routes:
        - service: web
          host: ${APP_HOST}
          kind: web
          security_headers: true
        - service: web
          host: api.${APP_HOST}
          path: /v1
          timeout: 30s
  ephemeral:
    isolate: prefix
    url: https://${SPACE}.preview.${APP_HOST}
    ttl: 24h
    replicas: 1
`

func TestRemoteSourceResolvesLikeTheClient(t *testing.T) {
	raw := []byte(equivalenceDeployYAML)
	for _, space := range []string{"app-production", "pr-42"} {
		t.Run(space, func(t *testing.T) {
			assertSameResolution(t, raw, space, "prod")
		})
	}
	t.Run("existing fixture", func(t *testing.T) {
		assertSameResolution(t, []byte(sourceDeployYAML), "app-production", "prod")
	})
}

func TestCollectSendsOnlyReferencedVars(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	vars, err := deployvars.Collect([]byte(equivalenceDeployYAML), fakeLookup)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := vars["SPACE"]; ok {
		t.Fatal("SPACE must be left to the node")
	}
	for _, name := range []string{"API_TOKEN", "APP_HOST", "COMMENTED_VAR", "DB_PASS", "REGISTRY_TOKEN", "SHARED_WEB", "SHARED_WORKER"} {
		if _, ok := vars[name]; !ok {
			t.Errorf("missing %s", name)
		}
	}
	if len(vars) != 7 {
		t.Fatalf("sent %d vars, want exactly the 7 referenced: %v", len(vars), vars)
	}
}

func TestCollectNamesEveryUnsetVar(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_, err := deployvars.Collect([]byte("services:\n  a:\n    image: x\n    env:\n      A: ${UNSET_B}\n      B: ${UNSET_A}\n"), fakeLookup)
	if err == nil || !strings.Contains(err.Error(), "UNSET_A, UNSET_B") {
		t.Fatalf("want both unset names, got %v", err)
	}
}
