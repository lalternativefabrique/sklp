package spacemanager

import (
	"strings"
	"testing"
)

const sourceDeployYAML = `
version: 1
kind: deploy
name: prod

registries:
  rg.example.com:
    username: nologin
    password: ${REGISTRY_TOKEN}

services:
  postgres:
    image: postgres:17-alpine
    port: 5432
    env:
      POSTGRES_PASSWORD: ${DB_PASS}
    volumes:
      - pgdata:/var/lib/postgresql/data
  web:
    image: rg.example.com/app/web:1
    port: 5273
    depends_on: [postgres]
    env:
      BETTER_AUTH_URL: https://${APP_HOST}

jobs:
  migrate:
    image: rg.example.com/app/migrate:1
    command: ["postgres://app:${DB_PASS}@postgres:5432/app", "up"]
    depends_on: [postgres]

spaces:
  app-production:
    gateway:
      routes:
        - service: web
          host: ${APP_HOST}
          security_headers: true
        - service: web
          host: app.${APP_HOST}
`

func TestResolveSourceIsTheLocalDeployPath(t *testing.T) {
	vars := map[string]string{
		"REGISTRY_TOKEN": "tok",
		"DB_PASS":        "p4ss",
		"APP_HOST":       "example.org",
	}
	services, meta, err := ResolveSource([]byte(sourceDeployYAML), vars, "app-production", "prod")
	if err != nil {
		t.Fatalf("ResolveSource: %v", err)
	}
	byName := map[string]Service{}
	for _, s := range services {
		byName[s.Name] = s
	}
	if len(byName) != 3 {
		t.Fatalf("services = %v, want postgres, web, migrate", byName)
	}
	if !byName["migrate"].Oneshot || byName["migrate"].Command[0] != "postgres://app:p4ss@postgres:5432/app" {
		t.Fatalf("migrate job = %+v", byName["migrate"])
	}
	if got := byName["postgres"].Volumes; len(got) != 1 || got[0] != "pgdata:/var/lib/postgresql/data" {
		t.Fatalf("postgres volumes = %v", got)
	}
	if len(meta.Routes) != 2 || meta.Routes[0].Host != "example.org" || meta.Routes[1].Host != "app.example.org" || !meta.Routes[0].SecurityHeaders {
		t.Fatalf("routes = %+v", meta.Routes)
	}
	if meta.Name != "app-production" || meta.Stack != "prod" {
		t.Fatalf("meta = %+v", meta)
	}
	if meta.EnvRefs["POSTGRES_PASSWORD"] != "${DB_PASS}" || meta.EnvValues["DB_PASS"] != "p4ss" {
		t.Fatalf("EnvRefs = %v EnvValues = %v, want POSTGRES_PASSWORD -> ${DB_PASS} carried as p4ss", meta.EnvRefs, meta.EnvValues)
	}
	reg := meta.Registries["rg.example.com"]
	if reg.Username != "nologin" || reg.PasswordEnv != "REGISTRY_TOKEN" || reg.Password != "tok" {
		t.Fatalf("registry auth = %+v", reg)
	}
}

func TestResolveSourceNeverReadsTheProcessEnv(t *testing.T) {
	t.Setenv("DB_PASS", "from-env")
	t.Setenv("REGISTRY_TOKEN", "from-env")
	t.Setenv("APP_HOST", "from-env")
	_, _, err := ResolveSource([]byte(sourceDeployYAML), map[string]string{"APP_HOST": "x", "REGISTRY_TOKEN": "t"}, "app-production", "prod")
	if err == nil || !strings.Contains(err.Error(), "DB_PASS") {
		t.Fatalf("err = %v, want the unresolved DB_PASS named", err)
	}
}
