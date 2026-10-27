package spacemanager

import (
	"strings"
	"testing"
	"time"
)

const domainsTwoSpacesYAML = `
services:
  core:
    image: core:latest
    expose: "4000"

spaces:
  prod:
    gateway:
      routes:
        - service: core
          host: api.example.com
  staging:
    gateway:
      routes:
        - service: core
          host: api.staging.acme.app
`

// findResolvedRoute returns the resolved route for a service in a SpaceMeta.
func findResolvedRoute(t *testing.T, meta SpaceMeta, service string) ResolvedRoute {
	t.Helper()
	for _, r := range meta.Routes {
		if r.Service == service {
			return r
		}
	}
	t.Fatalf("no resolved route for service %q in %+v", service, meta.Routes)
	return ResolvedRoute{}
}

func TestGatewayRoutes_PerSpaceResolvesRoute(t *testing.T) {
	cfg := parseIngressFixture(t, domainsTwoSpacesYAML)

	_, prodMeta, err := cfg.Resolve("prod", GatewayModeSingleton)
	if err != nil {
		t.Fatalf("Resolve(prod): %v", err)
	}
	if got := findResolvedRoute(t, prodMeta, "core").Host; got != "api.example.com" {
		t.Fatalf("prod core route host: want api.example.com, got %q", got)
	}

	_, stagingMeta, err := cfg.Resolve("staging", GatewayModeSingleton)
	if err != nil {
		t.Fatalf("Resolve(staging): %v", err)
	}
	if got := findResolvedRoute(t, stagingMeta, "core").Host; got != "api.staging.acme.app" {
		t.Fatalf("staging core route host: want api.staging.acme.app, got %q", got)
	}
}

func TestGatewayRoutes_UnknownService_Errors(t *testing.T) {
	body := `
services:
  core:
    image: core:latest
    expose: "4000"

spaces:
  prod:
    gateway:
      routes:
        - service: nope
          host: api.example.com
`
	_, err := Parse([]byte(body))
	if err == nil {
		t.Fatal("want unknown-service error, got nil")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Fatalf("error should name the unknown service, got: %v", err)
	}
}

func TestGatewayRoutes_ServiceWithoutPort_Errors(t *testing.T) {
	body := `
services:
  core:
    image: core:latest

spaces:
  prod:
    gateway:
      routes:
        - service: core
          host: api.example.com
`
	_, err := Parse([]byte(body))
	if err == nil {
		t.Fatal("want missing-port error, got nil")
	}
	if !strings.Contains(err.Error(), "port") {
		t.Fatalf("error should mention port, got: %v", err)
	}
}

func TestGatewayRoutes_InvalidFQDN_Errors(t *testing.T) {
	cases := map[string]string{
		"empty":       "",
		"leading dot": ".api.example.com",
		"bad chars":   "api_beta.app",
		"too long":    strings.Repeat("a", 64) + ".acme.app",
	}
	for name, host := range cases {
		t.Run(name, func(t *testing.T) {
			body := `
services:
  core:
    image: core:latest
    expose: "4000"

spaces:
  prod:
    gateway:
      routes:
        - service: core
          host: "` + host + `"
`
			_, err := Parse([]byte(body))
			if err == nil {
				t.Fatalf("want invalid-FQDN error for %q, got nil", host)
			}
		})
	}
}

func TestGatewayRoutes_HostnameReusedAcrossSpaces_Errors(t *testing.T) {
	body := `
services:
  core:
    image: core:latest
    expose: "4000"

spaces:
  prod:
    gateway:
      routes:
        - service: core
          host: api.example.com
  staging:
    gateway:
      routes:
        - service: core
          host: api.example.com
`
	_, err := Parse([]byte(body))
	if err == nil {
		t.Fatal("want hostname-reuse error, got nil")
	}
	if !strings.Contains(err.Error(), "prod") || !strings.Contains(err.Error(), "staging") {
		t.Fatalf("error should name both claimants, got: %v", err)
	}
}

func TestGatewayRoutes_IngressAndRouteConflict_Errors(t *testing.T) {
	body := `
services:
  core:
    image: core:latest
    expose: "4000"

spaces:
  prod:
    ingress:
      core: "4100:4000"
    gateway:
      routes:
        - service: core
          host: api.example.com
`
	_, err := Parse([]byte(body))
	if err == nil {
		t.Fatal("want ingress/gateway conflict error, got nil")
	}
	if !strings.Contains(err.Error(), "core") {
		t.Fatalf("error should name the conflicting service, got: %v", err)
	}
}

func TestGatewayRoutes_EphemeralFixedRoute_Errors(t *testing.T) {
	body := `
services:
  core:
    image: core:latest
    expose: "4000"

spaces:
  ephemeral:
    isolate: prefix
    url: "${SPACE}.preview.acme.app"
    gateway:
      routes:
        - service: core
          host: api.example.com
`
	_, err := Parse([]byte(body))
	if err == nil {
		t.Fatal("want ephemeral-fixed-route error, got nil")
	}
	if !strings.Contains(err.Error(), "ephemeral") {
		t.Fatalf("error should mention ephemeral, got: %v", err)
	}
}

func TestGatewayRoutes_WildcardAllowed(t *testing.T) {
	body := `
services:
  core:
    image: core:latest
    expose: "4000"

spaces:
  prod:
    gateway:
      routes:
        - service: core
          host: "*.acme.app"
`
	cfg := parseIngressFixture(t, body)
	_, prodMeta, err := cfg.Resolve("prod", GatewayModeSingleton)
	if err != nil {
		t.Fatalf("Resolve(prod): %v", err)
	}
	if got := findResolvedRoute(t, prodMeta, "core").Host; got != "*.acme.app" {
		t.Fatalf("prod core route host: want *.acme.app, got %q", got)
	}
}

func TestGatewayRoutes_PathPrefixResolves(t *testing.T) {
	body := `
services:
  core:
    image: core:latest
    expose: "4000"

spaces:
  prod:
    gateway:
      routes:
        - service: core
          host: acme.app
          path: /api
`
	cfg := parseIngressFixture(t, body)
	_, prodMeta, err := cfg.Resolve("prod", GatewayModeSingleton)
	if err != nil {
		t.Fatalf("Resolve(prod): %v", err)
	}
	r := findResolvedRoute(t, prodMeta, "core")
	if r.Host != "acme.app" || r.Path != "/api" {
		t.Fatalf("prod core route: want host=acme.app path=/api, got %+v", r)
	}
}

// A route's `kind:` must reach the resolved model as real numbers, whatever the
// path is named — the whole point of the field is that a login endpoint called
// /api/v2/sessions or /graphql cannot be recognised by its path.
func TestGatewayRoutes_KindSuppliesRateLimit(t *testing.T) {
	cfg := parseIngressFixture(t, `
services:
  core:
    image: core:latest
    expose: "4000"
  web:
    image: web:latest
    expose: "3000"

spaces:
  prod:
    gateway:
      routes:
        - service: core
          host: api.example.com
          path: /api/v2/sessions
          kind: auth
        - service: web
          host: acme.app
`)
	_, meta, err := cfg.Resolve("prod", GatewayModeSingleton)
	if err != nil {
		t.Fatalf("Resolve(prod): %v", err)
	}

	auth := findResolvedRoute(t, meta, "core")
	if auth.Rate != 5 || auth.Burst != 10 {
		t.Errorf("kind: auth resolved to rate %v burst %d, want 5/10", auth.Rate, auth.Burst)
	}
	// A route declaring no kind must stay unlimited: the field adds a default
	// only where it is asked for, never behind the operator's back.
	plain := findResolvedRoute(t, meta, "web")
	if plain.Rate != 0 || plain.Burst != 0 {
		t.Errorf("route with no kind resolved to rate %v burst %d, want unlimited", plain.Rate, plain.Burst)
	}
}

// An explicit rate on a kinded route wins — the kind is a default, not a cap.
func TestGatewayRoutes_ExplicitRateBeatsKind(t *testing.T) {
	cfg := parseIngressFixture(t, `
services:
  core:
    image: core:latest
    expose: "4000"

spaces:
  prod:
    gateway:
      routes:
        - service: core
          host: api.example.com
          path: /oauth/token
          kind: auth
          rate: 2
          burst: 4
`)
	_, meta, err := cfg.Resolve("prod", GatewayModeSingleton)
	if err != nil {
		t.Fatalf("Resolve(prod): %v", err)
	}
	if r := findResolvedRoute(t, meta, "core"); r.Rate != 2 || r.Burst != 4 {
		t.Errorf("explicit rate resolved to %v/%d, want the declared 2/4", r.Rate, r.Burst)
	}
}

// An unknown kind must be refused at parse time. Ignoring it would leave the
// route unlimited while its author believes a limit is in place.
func TestGatewayRoutes_UnknownKind_Errors(t *testing.T) {
	_, err := Parse([]byte(`
services:
  core:
    image: core:latest
    expose: "4000"

spaces:
  prod:
    gateway:
      routes:
        - service: core
          host: api.example.com
          kind: login
`))
	if err == nil {
		t.Fatal("Parse accepted an unknown route kind, want an error")
	}
	if !strings.Contains(err.Error(), "unknown route kind") {
		t.Errorf("error %q should name the problem as an unknown route kind", err)
	}
}

// `timeout:` must reach the resolved model as a duration, and a route omitting
// it must stay unbounded — the cap is opt-in precisely so large transfers and
// WebSocket routes keep working.
func TestGatewayRoutes_TimeoutResolves(t *testing.T) {
	cfg := parseIngressFixture(t, `
services:
  core:
    image: core:latest
    expose: "4000"
  web:
    image: web:latest
    expose: "3000"

spaces:
  prod:
    gateway:
      routes:
        - service: core
          host: api.example.com
          path: /api
          kind: api
          timeout: 30s
        - service: web
          host: example.com
`)
	_, meta, err := cfg.Resolve("prod", GatewayModeSingleton)
	if err != nil {
		t.Fatalf("Resolve(prod): %v", err)
	}
	if got := findResolvedRoute(t, meta, "core").Timeout; got != 30*time.Second {
		t.Errorf("timeout resolved to %v, want 30s", got)
	}
	if got := findResolvedRoute(t, meta, "web").Timeout; got != 0 {
		t.Errorf("route without timeout resolved to %v, want unbounded (0)", got)
	}
}

func TestGatewayRoutes_InvalidTimeout_Errors(t *testing.T) {
	_, err := Parse([]byte(`
services:
  core:
    image: core:latest
    expose: "4000"
spaces:
  prod:
    gateway:
      routes:
        - service: core
          host: api.example.com
          timeout: soon
`))
	if err == nil {
		t.Fatal("Parse accepted an invalid timeout, want an error")
	}
	if !strings.Contains(err.Error(), "invalid timeout") {
		t.Errorf("error %q should name the invalid timeout", err)
	}
}

// max_body must reach the resolved model as bytes, and a route omitting it must
// stay unbounded.
func TestGatewayRoutes_MaxBodyResolves(t *testing.T) {
	cfg := parseIngressFixture(t, `
services:
  core:
    image: core:latest
    expose: "4000"
  web:
    image: web:latest
    expose: "3000"

spaces:
  prod:
    gateway:
      routes:
        - service: core
          host: api.example.com
          path: /api
          kind: api
          max_body: 1Mi
        - service: web
          host: example.com
          kind: web
`)
	_, meta, err := cfg.Resolve("prod", GatewayModeSingleton)
	if err != nil {
		t.Fatalf("Resolve(prod): %v", err)
	}
	if got := findResolvedRoute(t, meta, "core").MaxBody; got != 1<<20 {
		t.Errorf("max_body resolved to %d, want %d", got, 1<<20)
	}
	// kind: web must NOT imply a body cap.
	if got := findResolvedRoute(t, meta, "web").MaxBody; got != 0 {
		t.Errorf("kind alone resolved max_body to %d, want unbounded — a body cap must never be inherited", got)
	}
}

func TestGatewayRoutes_InvalidMaxBody_Errors(t *testing.T) {
	_, err := Parse([]byte(`
services:
  core:
    image: core:latest
    expose: "4000"
spaces:
  prod:
    gateway:
      routes:
        - service: core
          host: api.example.com
          max_body: enormous
`))
	if err == nil {
		t.Fatal("Parse accepted an invalid max_body, want an error")
	}
	if !strings.Contains(err.Error(), "invalid max_body") {
		t.Errorf("error %q should name the invalid max_body", err)
	}
}
