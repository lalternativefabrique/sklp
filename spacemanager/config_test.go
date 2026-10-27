package spacemanager

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fixtureYAML = `
services:
  api:
    image: rg.fr-par.scw.cloud/skalpai/api:${TAG}
    replicas: 1
    expose: "8080"
    env:
      DATABASE_URL: "postgres://app:${PG_PASSWORD}@db:5432/app"
      NODE_ENV: production
    ready:
      http: "http://localhost:8080/health"
      timeout: 20s
    depends_on: [db]
  db:
    image: postgres:16
    env: { POSTGRES_PASSWORD: "${PG_PASSWORD}" }
    volumes: ["pgdata:/var/lib/postgresql/data"]
    ready: { tcp: "localhost:5432", timeout: 30s }
  proxy:
    image: traefik:v3
    proxy: true
    ports: ["443:443", "80:80"]

spaces:
  production: { api: { replicas: 3 } }
  staging:    { api: { replicas: 1 } }
  ephemeral:
    replicas: 1
    isolate: prefix
    url: "${SPACE}.preview.skalpai.io"
    ttl: 72h
`

func parseFixture(t *testing.T) *DeployConfig {
	t.Helper()
	t.Setenv("TAG", "v1.2.3")
	t.Setenv("PG_PASSWORD", "s3cr3t")
	cfg, err := Parse([]byte(fixtureYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return cfg
}

func TestParse_ServicesAndMapping(t *testing.T) {
	cfg := parseFixture(t)

	if got := len(cfg.Services); got != 3 {
		t.Fatalf("services: want 3, got %d", got)
	}

	api := cfg.Services["api"]
	if api.Image != "rg.fr-par.scw.cloud/skalpai/api:v1.2.3" {
		t.Errorf("api image substitution failed: %q", api.Image)
	}
	svc := api.toService("api")
	wantEnv := []string{"DATABASE_URL=postgres://app:s3cr3t@db:5432/app", "NODE_ENV=production"}
	if len(svc.Env) != len(wantEnv) {
		t.Fatalf("api env: want %v, got %v", wantEnv, svc.Env)
	}
	for i := range wantEnv {
		if svc.Env[i] != wantEnv[i] {
			t.Errorf("api env[%d]: want %q got %q", i, wantEnv[i], svc.Env[i])
		}
	}
	// `expose: "8080"` is the deprecated alias; it must resolve into Port.
	if svc.Port != 8080 {
		t.Errorf("api port (via expose alias): want 8080, got %d", svc.Port)
	}
	if svc.Replicas != 1 {
		t.Errorf("api replicas: want 1, got %d", svc.Replicas)
	}
	if svc.Ready == nil || svc.Ready.HTTP != "http://localhost:8080/health" || svc.Ready.Timeout != "20s" {
		t.Errorf("api ready probe mapping wrong: %+v", svc.Ready)
	}
}

func TestParse_PortField(t *testing.T) {
	cfg, err := Parse([]byte("services:\n  api:\n    image: img:1\n    port: 8080\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.Services["api"].Port; got != 8080 {
		t.Fatalf("port: want 8080, got %d", got)
	}
}

func TestParse_ExposeAliasResolvesToPort(t *testing.T) {
	cfg, err := Parse([]byte("services:\n  api:\n    image: img:1\n    expose: \"8080\"\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	api := cfg.Services["api"]
	if api.Port != 8080 {
		t.Fatalf("expose alias: want Port 8080, got %d", api.Port)
	}
	if api.Expose != "" {
		t.Fatalf("expose alias should be cleared after normalize, got %q", api.Expose)
	}
}

func TestParse_PortWinsOverExposeAlias(t *testing.T) {
	cfg, err := Parse([]byte("services:\n  api:\n    image: img:1\n    port: 9000\n    expose: \"8080\"\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.Services["api"].Port; got != 9000 {
		t.Fatalf("port must win over expose alias: want 9000, got %d", got)
	}
}

func TestParse_NonNumericExpose_Errors(t *testing.T) {
	_, err := Parse([]byte("services:\n  api:\n    image: img:1\n    expose: \"http\"\n"))
	if err == nil {
		t.Fatal("want error for non-numeric expose, got nil")
	}
	if !strings.Contains(err.Error(), "expose") {
		t.Fatalf("error should mention expose, got: %v", err)
	}
}

func TestParse_EnvSliceSortedDeterministic(t *testing.T) {
	got := envSlice(map[string]string{"Z": "1", "A": "2", "M": "3"})
	want := []string{"A=2", "M=3", "Z=1"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("envSlice not sorted: %v", got)
		}
	}
}

func TestParse_ProxyAndReady(t *testing.T) {
	cfg := parseFixture(t)
	if !cfg.Services["proxy"].Proxy {
		t.Error("proxy service should have proxy: true")
	}
	if cfg.ProxyServiceName() != "proxy" {
		t.Errorf("ProxyServiceName: want proxy, got %q", cfg.ProxyServiceName())
	}
	db := cfg.Services["db"].toService("db")
	if db.Ready == nil || db.Ready.TCP != "localhost:5432" || db.Ready.Timeout != "30s" {
		t.Errorf("db ready tcp mapping wrong: %+v", db.Ready)
	}
}

func TestParse_MissingVarIsError(t *testing.T) {
	t.Setenv("TAG", "v1")
	// PG_PASSWORD deliberately unset.
	_, err := Parse([]byte(fixtureYAML))
	if err == nil {
		t.Fatal("want error for unset PG_PASSWORD, got nil")
	}
	if !strings.Contains(err.Error(), "PG_PASSWORD") {
		t.Errorf("error should name the missing var, got: %v", err)
	}
}

func TestParse_SpacePlaceholderKeptVerbatim(t *testing.T) {
	cfg := parseFixture(t)
	// ${SPACE} must survive load-time expansion; it is filled in Resolve.
	if !strings.Contains(cfg.Spaces["ephemeral"].URL, "${SPACE}") {
		t.Errorf("SPACE placeholder should be kept verbatim, got %q", cfg.Spaces["ephemeral"].URL)
	}
}

func TestExpandVars_OnlyBracedForm(t *testing.T) {
	t.Setenv("FOO", "bar")
	// A command carrying shell syntax ($?, $2, $(date)) must pass through
	// untouched; only the braced ${FOO} is resolved.
	in := `services:
  a:
    image: img
    command: ["sh", "-c", "exit $? || echo $2 $(date) ${FOO}"]`
	got, err := expandVars(in)
	if err != nil {
		t.Fatalf("expandVars: %v", err)
	}
	for _, raw := range []string{"$?", "$2", "$(date)"} {
		if !strings.Contains(got, raw) {
			t.Errorf("bare shell token %q was altered; got: %s", raw, got)
		}
	}
	if !strings.Contains(got, "bar") || strings.Contains(got, "${FOO}") {
		t.Errorf("${FOO} not resolved to bar; got: %s", got)
	}
}

func TestExpandVars_MissingBracedIsError(t *testing.T) {
	// A referenced ${MISSING} with no env value is still a hard error.
	if _, err := expandVars("x: ${MISSING}"); err == nil || !strings.Contains(err.Error(), "MISSING") {
		t.Fatalf("want unset-var error naming MISSING, got %v", err)
	}
	// A bare $MISSING is NOT a reference and must not error.
	if _, err := expandVars("x: $MISSING and $2"); err != nil {
		t.Fatalf("bare $VAR must not be treated as a reference: %v", err)
	}
}

func TestValidate_TwoProxies(t *testing.T) {
	const y = `
services:
  a: { image: x, proxy: true }
  b: { image: y, proxy: true }
`
	if _, err := Parse([]byte(y)); err == nil || !strings.Contains(err.Error(), "proxy") {
		t.Fatalf("want two-proxy error, got %v", err)
	}
}

func TestValidate_UnknownDependsOn(t *testing.T) {
	const y = `
services:
  a: { image: x, depends_on: [ghost] }
`
	if _, err := Parse([]byte(y)); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("want unknown depends_on error, got %v", err)
	}
}

func TestValidate_CrossSpaceDependsOnRejected(t *testing.T) {
	const y = `
services:
  a: { image: x, depends_on: ["other-space/nats"] }
`
	_, err := Parse([]byte(y))
	if err == nil || !strings.Contains(err.Error(), "cross-Space") {
		t.Fatalf("want a cross-Space depends_on error, got %v", err)
	}
}

func TestValidate_Cycle(t *testing.T) {
	const y = `
services:
  a: { image: x, depends_on: [b] }
  b: { image: y, depends_on: [a] }
`
	if _, err := Parse([]byte(y)); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("want cycle error, got %v", err)
	}
}

func TestValidate_MissingImage(t *testing.T) {
	const y = `
services:
  a: { replicas: 1 }
`
	if _, err := Parse([]byte(y)); err == nil || !strings.Contains(err.Error(), "image") {
		t.Fatalf("want missing-image error, got %v", err)
	}
}

func TestResolve_NamedSpaceOverride(t *testing.T) {
	cfg := parseFixture(t)
	svcs, meta, err := cfg.Resolve("production", GatewayModeSingleton)
	if err != nil {
		t.Fatalf("Resolve(production): %v", err)
	}
	if meta.Ephemeral {
		t.Error("production must not be ephemeral")
	}
	api := findSvc(t, svcs, "api")
	if api.Replicas != 3 {
		t.Errorf("production api replicas: want 3, got %d", api.Replicas)
	}
	// db untouched.
	db := findSvc(t, svcs, "db")
	if db.Replicas != 0 {
		t.Errorf("db replicas should be unset (0), got %d", db.Replicas)
	}
}

func TestResolve_StagingOverride(t *testing.T) {
	cfg := parseFixture(t)
	svcs, _, err := cfg.Resolve("staging", GatewayModeSingleton)
	if err != nil {
		t.Fatalf("Resolve(staging): %v", err)
	}
	if findSvc(t, svcs, "api").Replicas != 1 {
		t.Errorf("staging api replicas: want 1")
	}
}

func TestResolve_EphemeralMould(t *testing.T) {
	cfg := parseFixture(t)
	svcs, meta, err := cfg.Resolve("pr-1234", GatewayModeSingleton)
	if err != nil {
		t.Fatalf("Resolve(pr-1234): %v", err)
	}
	if !meta.Ephemeral {
		t.Error("pr-1234 should be ephemeral")
	}
	if meta.Prefix != "pr-1234-" {
		t.Errorf("prefix: want pr-1234-, got %q", meta.Prefix)
	}
	if meta.URL != "pr-1234.preview.skalpai.io" {
		t.Errorf("SPACE substitution: want pr-1234.preview.skalpai.io, got %q", meta.URL)
	}
	if meta.TTL != 72*time.Hour {
		t.Errorf("ttl: want 72h, got %v", meta.TTL)
	}
	// Every service name is prefixed.
	for _, want := range []string{"pr-1234-api", "pr-1234-db", "pr-1234-proxy"} {
		findSvc(t, svcs, want)
	}
	// Mould replicas override applies.
	if findSvc(t, svcs, "pr-1234-api").Replicas != 1 {
		t.Errorf("ephemeral api replicas: want 1")
	}
}

func TestResolve_UnknownSpaceNoMould(t *testing.T) {
	const y = `
services:
  a: { image: x }
spaces:
  production: { a: { replicas: 2 } }
`
	t.Setenv("TAG", "x")
	cfg, err := Parse([]byte(y))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, _, err := cfg.Resolve("pr-9", GatewayModeSingleton); err == nil || !strings.Contains(err.Error(), "ephemeral") {
		t.Fatalf("want no-mould error, got %v", err)
	}
}

func TestResolve_EmptySpaceName(t *testing.T) {
	cfg := parseFixture(t)
	if _, _, err := cfg.Resolve("", GatewayModeSingleton); err == nil {
		t.Fatal("want error for empty space name")
	}
}

func findSvc(t *testing.T, svcs []Service, name string) Service {
	t.Helper()
	for _, s := range svcs {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("service %q not found in %d resolved services", name, len(svcs))
	return Service{}
}

// --- Section A: jobs parsing / resolution ---

const jobsYAML = `
services:
  postgres:
    image: postgres:16
    ready: { tcp: "postgres:5432" }
  core:
    image: acme/core:local
    depends_on: [postgres, migrate]
    ready: { http: "http://core:4000/health" }

jobs:
  migrate:
    image: acme/migrate:local
    depends_on: [postgres]
    command: ["up"]
`

func parseJobs(t *testing.T) *DeployConfig {
	t.Helper()
	cfg, err := Parse([]byte(jobsYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return cfg
}

func TestParse_JobsParsedAsOneshot(t *testing.T) {
	cfg := parseJobs(t)
	svcs, _, err := cfg.Resolve("production", GatewayModeSingleton)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	migrate := findSvc(t, svcs, "migrate")
	if !migrate.Oneshot {
		t.Error("job migrate must resolve with Oneshot=true")
	}
	core := findSvc(t, svcs, "core")
	if core.Oneshot {
		t.Error("service core must resolve with Oneshot=false")
	}
}

func TestParse_JobDependsOnCopied(t *testing.T) {
	cfg := parseJobs(t)
	svcs, _, err := cfg.Resolve("production", GatewayModeSingleton)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	migrate := findSvc(t, svcs, "migrate")
	if len(migrate.DependsOn) != 1 || migrate.DependsOn[0] != "postgres" {
		t.Fatalf("job depends_on not copied: %v", migrate.DependsOn)
	}
}

func TestParse_ServiceDependsOnJob(t *testing.T) {
	cfg := parseJobs(t)
	svcs, _, err := cfg.Resolve("production", GatewayModeSingleton)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	core := findSvc(t, svcs, "core")
	found := false
	for _, d := range core.DependsOn {
		if d == "migrate" {
			found = true
		}
	}
	if !found {
		t.Fatalf("core depends_on should include the job migrate: %v", core.DependsOn)
	}
}

func TestParse_DuplicateNameAcrossMaps(t *testing.T) {
	const y = `
services:
  dup: { image: x }
jobs:
  dup: { image: y }
`
	if _, err := Parse([]byte(y)); err == nil || !strings.Contains(err.Error(), "dup") {
		t.Fatalf("want duplicate-name-across-maps error naming dup, got %v", err)
	}
}

// --- Section B: union validation ---

func TestValidate_UnknownDependencyAcrossUnion(t *testing.T) {
	const y = `
services:
  a: { image: x, depends_on: [ghost] }
jobs:
  j: { image: y }
`
	if _, err := Parse([]byte(y)); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("want unknown-dependency error naming ghost, got %v", err)
	}
}

func TestValidate_DependsOnJobResolves(t *testing.T) {
	const y = `
services:
  a: { image: x, depends_on: [j] }
jobs:
  j: { image: y }
`
	if _, err := Parse([]byte(y)); err != nil {
		t.Fatalf("a depends_on job j must resolve, got %v", err)
	}
}

func TestValidate_CycleAcrossUnion(t *testing.T) {
	// service a -> job j -> service a, a cycle crossing the two families.
	const y = `
services:
  a: { image: x, depends_on: [j] }
jobs:
  j: { image: y, depends_on: [a] }
`
	if _, err := Parse([]byte(y)); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("want cross-family cycle error, got %v", err)
	}
}

func TestValidate_SelfCycleJob(t *testing.T) {
	const y = `
jobs:
  j: { image: y, depends_on: [j] }
services:
  a: { image: x }
`
	if _, err := Parse([]byte(y)); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("want self-cycle error on job j, got %v", err)
	}
}

// TestServiceSpec_ToService_CopiesVolumes guards the deploy-side volume wiring:
// a service's volumes: must reach the runnable Service (they were dropped before,
// so postgres/nats wrote into the read-only rootfs and crash-looped).
func TestServiceSpec_ToService_CopiesVolumes(t *testing.T) {
	spec := ServiceSpec{Image: "postgres:16", Volumes: []string{"pgdata:/var/lib/postgresql/data"}}
	svc := spec.toService("db")
	if len(svc.Volumes) != 1 || svc.Volumes[0] != "pgdata:/var/lib/postgresql/data" {
		t.Fatalf("toService dropped volumes: %v", svc.Volumes)
	}
}

// TestLoad_EnvFilesResolveVars: env_files declared in the deploy file are
// loaded into the process env BEFORE ${VAR} expansion, so a deploy referencing
// ${TAG} resolves from .env without the caller exporting it first. The absolute
// paths are recorded for the supervisor's EnvironmentFile=.
func TestLoad_EnvFilesResolveVars(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("TAG=42\nSECRET=s3cr3t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	deploy := `
env_files:
  - .env
registries:
  rg.example.com:
    username: nologin
    password: ${SECRET}
services:
  api:
    image: rg.example.com/api:${TAG}
    port: 8080
spaces:
  prod:
    gateway:
      routes:
        - service: api
          host: api.localhost
`
	p := filepath.Join(dir, "deploy.yaml")
	if err := os.WriteFile(p, []byte(deploy), 0o644); err != nil {
		t.Fatal(err)
	}
	// TAG/SECRET are NOT in the host env: they must come from .env via Load.
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Services["api"].Image; got != "rg.example.com/api:42" {
		t.Fatalf("Image = %q, want resolved from .env", got)
	}
	want := filepath.Join(dir, ".env")
	if rf := cfg.ResolvedEnvFiles(); len(rf) != 1 || rf[0] != want {
		t.Fatalf("ResolvedEnvFiles = %v, want [%s]", rf, want)
	}
}

// TestLoad_EnvFilesFromRepoRoot: a deploy at <root>/.sklp/<stack>/deploy.yaml
// resolves env_files against <root>, so the conventional .env at the repo root
// is found (not next to the deploy file two levels down).
func TestLoad_EnvFilesFromRepoRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("REPOROOTTAG=root7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, ".sklp", "prod")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	deploy := `
env_files:
  - .env
services:
  api:
    image: rg.example.com/api:${REPOROOTTAG}
    port: 8080
spaces:
  prod:
    gateway:
      routes:
        - service: api
          host: api.localhost
`
	p := filepath.Join(dir, "deploy.yaml")
	if err := os.WriteFile(p, []byte(deploy), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Services["api"].Image; got != "rg.example.com/api:root7" {
		t.Fatalf("Image = %q, want ${REPOROOTTAG} from the repo-root .env", got)
	}
	want := filepath.Join(root, ".env")
	if rf := cfg.ResolvedEnvFiles(); len(rf) != 1 || rf[0] != want {
		t.Fatalf("ResolvedEnvFiles = %v, want [%s]", rf, want)
	}
}

// TestParse_GatewayEmailExported: gateway.email is exported as
// SKLP_GATEWAY_ACME_EMAIL so the traefik driver gets a real ACME contact from
// the YAML; an existing env var wins.
func TestParse_GatewayEmailExported(t *testing.T) {
	t.Setenv("SKLP_GATEWAY_ACME_EMAIL", "") // start clean, unset below
	os.Unsetenv("SKLP_GATEWAY_ACME_EMAIL")
	yaml := `
gateway:
  email: ops@example.com
services:
  api:
    image: x/y:1
    port: 80
spaces:
  prod:
    gateway:
      routes:
        - service: api
          host: api.localhost
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.GatewayEmail() != "ops@example.com" {
		t.Fatalf("GatewayEmail = %q", cfg.GatewayEmail())
	}
	if got := os.Getenv("SKLP_GATEWAY_ACME_EMAIL"); got != "ops@example.com" {
		t.Fatalf("SKLP_GATEWAY_ACME_EMAIL = %q, want exported from gateway.email", got)
	}
}

func TestValidate_HostBindVolumeRejected(t *testing.T) {
	for _, y := range []string{`
services:
  a:
    image: x
    volumes: ["/home/architect/.config/sklp/control:/loot:ro"]
`, `
services:
  a: { image: x }
jobs:
  j:
    image: x
    volumes: ["/:/host"]
`, `
services:
  a:
    image: x
    volumes: ["..:/data"]
`} {
		if _, err := Parse([]byte(y)); err == nil || !strings.Contains(err.Error(), "volume") {
			t.Errorf("want volume error, got %v", err)
		}
	}
	const ok = `
services:
  a:
    image: x
    volumes: ["pgdata:/var/lib/postgresql/data", "dumps:/backups:ro"]
`
	if _, err := Parse([]byte(ok)); err != nil {
		t.Fatalf("named volumes must parse: %v", err)
	}
}
