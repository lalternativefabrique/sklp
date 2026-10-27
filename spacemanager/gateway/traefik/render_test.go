package traefik

import (
	"strings"
	"testing"
	"time"
)

func TestRenderRoutes_TLS(t *testing.T) {
	got := renderRoutes("production", "core", []route{
		{Host: "api.example.com", Backends: []string{"127.0.0.1:40002", "127.0.0.1:40001"}, TLS: true},
	})
	for _, sub := range []string{
		// Single route: unsuffixed "<space>-<service>" name.
		"production-core:",
		"Host(`api.example.com`)",
		"entryPoints: [websecure]",
		"tls:",
		"certResolver: le",
		// Backends sorted deterministically.
		`url: "http://127.0.0.1:40001"`,
		`url: "http://127.0.0.1:40002"`,
	} {
		if !strings.Contains(got, sub) {
			t.Errorf("renderRoutes(TLS) missing %q\n%s", sub, got)
		}
	}
	if strings.Index(got, "40001") > strings.Index(got, "40002") {
		t.Errorf("backends not sorted:\n%s", got)
	}
}

func TestRenderRoutes_NoTLS(t *testing.T) {
	got := renderRoutes("dev", "web", []route{
		{Host: "app.local", Backends: []string{"127.0.0.1:5000"}},
	})
	if !strings.Contains(got, "entryPoints: [web]") {
		t.Errorf("non-TLS router not on web entrypoint:\n%s", got)
	}
	if strings.Contains(got, "tls:") {
		t.Errorf("non-TLS router must omit tls:\n%s", got)
	}
}

func TestRenderRoutes_EmptyBackends(t *testing.T) {
	got := renderRoutes("production", "core", []route{
		{Host: "api.example.com", TLS: true},
	})
	if !strings.Contains(got, "Host(`api.example.com`)") {
		t.Errorf("router missing host rule:\n%s", got)
	}
	if strings.Contains(got, "url:") {
		t.Errorf("empty-backend router must carry no server url:\n%s", got)
	}
}

func TestRenderRoutes_PathPrefix(t *testing.T) {
	got := renderRoutes("production", "core", []route{
		{Host: "api.example.com", Path: "/api", Backends: []string{"127.0.0.1:40001"}, TLS: true},
	})
	if !strings.Contains(got, "Host(`api.example.com`) && PathPrefix(`/api`)") {
		t.Errorf("route with path missing PathPrefix rule:\n%s", got)
	}
}

func TestRenderRoutes_MultiRoute(t *testing.T) {
	got := renderRoutes("production", "core", []route{
		{Host: "api.example.com", Backends: []string{"127.0.0.1:40001"}, TLS: true},
		{Host: "acme.app", Path: "/api", Backends: []string{"127.0.0.1:40001"}, TLS: true},
	})
	for _, sub := range []string{
		// >1 route: per-route suffixed names.
		"production-core-0:",
		"production-core-1:",
		"Host(`api.example.com`)",
		"Host(`acme.app`) && PathPrefix(`/api`)",
	} {
		if !strings.Contains(got, sub) {
			t.Errorf("multi-route render missing %q\n%s", sub, got)
		}
	}
	if strings.Contains(got, "production-core:") {
		t.Errorf("multi-route must NOT emit the unsuffixed name:\n%s", got)
	}
}

func TestRenderStaticConfig(t *testing.T) {
	got := renderStaticConfig("/etc/traefik/dynamic", "/etc/traefik/acme.json", "/etc/traefik/traefik.log", "ops@example.com", "")
	for _, sub := range []string{
		`address: ":9080"`,
		"redirections",
		"scheme: https",
		`to: ":443"`,
		`address: ":9443"`,
		`directory: "/etc/traefik/dynamic"`,
		"watch: true",
		"acme:",
		`email: "ops@example.com"`,
		`storage: "/etc/traefik/acme.json"`,
		"httpChallenge:",
		"dashboard: false",
		"checkNewVersion: false",
		`filePath: "/etc/traefik/traefik.log"`,
	} {
		if !strings.Contains(got, sub) {
			t.Errorf("static config missing %q\n%s", sub, got)
		}
	}
}

func TestRenderStaticConfig_DefaultEmail(t *testing.T) {
	got := renderStaticConfig("/d", "/a.json", "/l.log", "", "")
	if !strings.Contains(got, `email: "admin@example.com"`) {
		t.Errorf("empty email did not default:\n%s", got)
	}
}

func TestRenderStaticConfig_DNSChallenge(t *testing.T) {
	got := renderStaticConfig("/etc/traefik/dynamic", "/etc/traefik/acme.json", "/etc/traefik/traefik.log", "ops@example.com", "ovh")
	for _, sub := range []string{
		"dnsChallenge:",
		"provider: ovh",
	} {
		if !strings.Contains(got, sub) {
			t.Errorf("dnsChallenge config missing %q\n%s", sub, got)
		}
	}
	if strings.Contains(got, "httpChallenge:") {
		t.Errorf("dnsChallenge config must not also emit httpChallenge:\n%s", got)
	}
}

// TestRenderRoutes_MeshBackendHost: a mesh route's rendered config carries the
// Host-rewrite middleware so the remote controller routes by internal name.
func TestRenderRoutes_MeshBackendHost(t *testing.T) {
	out := renderRoutes("front", "alpha/web", []route{{
		Host:        "b.example.com",
		Backends:    []string{"10.90.0.7:80"},
		TLS:         true,
		BackendHost: "web.alpha.internal",
	}})
	for _, want := range []string{
		`url: "http://10.90.0.7:80"`,
		"customRequestHeaders:",
		`Host: "web.alpha.internal"`,
		"front-alpha_web-meshhost",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered config missing %q:\n%s", want, out)
		}
	}
}

func TestRenderRoutes_RateLimit(t *testing.T) {
	got := renderRoutes("production", "core", []route{
		{Host: "api.example.com", Backends: []string{"127.0.0.1:40001"}, TLS: true, Rate: 10, Burst: 20},
	})
	for _, sub := range []string{
		"production-core-ratelimit:",
		"rateLimit:",
		"average: 10",
		"burst: 20",
		"middlewares: [production-core-ratelimit]",
	} {
		if !strings.Contains(got, sub) {
			t.Errorf("renderRoutes(rate) missing %q\n%s", sub, got)
		}
	}
	// A whole-second rate needs no period: traefik's default is already 1s.
	if strings.Contains(got, "period:") {
		t.Errorf("integer rate must not emit a period:\n%s", got)
	}
}

// A route with no rate must carry no rateLimit middleware at all — an
// unlimited route is the default and must stay untouched.
func TestRenderRoutes_NoRateLimitByDefault(t *testing.T) {
	got := renderRoutes("production", "core", []route{
		{Host: "api.example.com", Backends: []string{"127.0.0.1:40001"}, TLS: true},
	})
	if strings.Contains(got, "rateLimit") {
		t.Errorf("unlimited route must emit no rateLimit middleware:\n%s", got)
	}
}

// A sub-1 rps rate must be scaled into a longer period, never truncated to
// average: 0 — traefik reads a zero average as "no limit", the exact inverse
// of the declaration.
func TestRenderRoutes_FractionalRateScalesPeriod(t *testing.T) {
	got := renderRoutes("production", "core", []route{
		{Host: "api.example.com", Backends: []string{"127.0.0.1:40001"}, Rate: 0.5, Burst: 2},
	})
	if !strings.Contains(got, "average: 1") || !strings.Contains(got, "period: 2s") {
		t.Errorf("0.5 rps must render as 1 per 2s:\n%s", got)
	}
	if strings.Contains(got, "average: 0") {
		t.Errorf("fractional rate truncated to average: 0 (traefik reads it as unlimited):\n%s", got)
	}
}

func TestRateLimitAverage(t *testing.T) {
	cases := []struct {
		rps     float64
		average int64
		period  string
	}{
		{10, 10, ""},
		{1, 1, ""},
		{2.7, 2, ""},
		{0.5, 1, "2s"},
		{0.1, 1, "10s"},
		{0.3, 1, "4s"},
	}
	for _, c := range cases {
		avg, period := rateLimitAverage(c.rps)
		if avg != c.average || period != c.period {
			t.Errorf("rateLimitAverage(%v) = (%d, %q), want (%d, %q)", c.rps, avg, period, c.average, c.period)
		}
	}
}

// An absurdly small rate must not render a negative period: traefik rejects the
// middleware, and a rejected file unroutes every route of the service.
func TestRateLimitAverage_ClampsExtremeRate(t *testing.T) {
	for _, rps := range []float64{1e-9, 1e-300} {
		avg, period := rateLimitAverage(rps)
		if avg != 1 || period != "86400s" {
			t.Errorf("rateLimitAverage(%v) = (%d, %q), want (1, \"86400s\")", rps, avg, period)
		}
	}
}

func TestRenderRoutes_Timeout(t *testing.T) {
	got := renderRoutes("production", "core", []route{
		{Host: "api.example.com", Backends: []string{"127.0.0.1:40001"}, TLS: true, Timeout: 30 * time.Second},
	})
	for _, sub := range []string{
		"serversTransports:",
		"production-core-transport:",
		"forwardingTimeouts:",
		"dialTimeout: 30s",
		"responseHeaderTimeout: 30s",
		"serversTransport: production-core-transport",
	} {
		if !strings.Contains(got, sub) {
			t.Errorf("renderRoutes(timeout) missing %q\n%s", sub, got)
		}
	}
	// The servers list must still be there and well-formed under loadBalancer.
	if !strings.Contains(got, "servers:") || !strings.Contains(got, `url: "http://127.0.0.1:40001"`) {
		t.Errorf("timeout render broke the loadBalancer servers block:\n%s", got)
	}
}

// A route with no timeout must emit no serversTransport at all.
func TestRenderRoutes_NoTimeoutNoTransport(t *testing.T) {
	got := renderRoutes("production", "core", []route{
		{Host: "api.example.com", Backends: []string{"127.0.0.1:40001"}, TLS: true},
	})
	if strings.Contains(got, "serversTransport") {
		t.Errorf("route without timeout must emit no serversTransport:\n%s", got)
	}
}

// Sub-second values round UP: traefik would read "0s" as no timeout, the
// inverse of the declaration.
func TestDurationSeconds(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{90 * time.Second, "90s"},
		{2 * time.Minute, "120s"},
		{1500 * time.Millisecond, "2s"},
		{100 * time.Millisecond, "1s"},
		{time.Nanosecond, "1s"},
	}
	for _, c := range cases {
		if got := durationSeconds(c.in); got != c.want {
			t.Errorf("durationSeconds(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRenderRoutes_MaxBody(t *testing.T) {
	got := renderRoutes("production", "core", []route{
		{Host: "api.example.com", Backends: []string{"127.0.0.1:40001"}, TLS: true, MaxBody: 10 << 20},
	})
	for _, sub := range []string{
		"production-core-maxbody:",
		"buffering:",
		"maxRequestBodyBytes: 10485760",
		"middlewares: [production-core-maxbody]",
	} {
		if !strings.Contains(got, sub) {
			t.Errorf("renderRoutes(maxBody) missing %q\n%s", sub, got)
		}
	}
	// Above traefik's 1 MiB memory default, memRequestBodyBytes is left alone.
	if strings.Contains(got, "memRequestBodyBytes") {
		t.Errorf("a cap above the memory default must not restate memRequestBodyBytes:\n%s", got)
	}
}

// A cap below traefik's 1 MiB memory default must lower memRequestBodyBytes
// too, so a small cap never reserves more memory than it can possibly use.
func TestRenderRoutes_MaxBodyBelowMemDefault(t *testing.T) {
	got := renderRoutes("production", "core", []route{
		{Host: "api.example.com", Backends: []string{"127.0.0.1:40001"}, MaxBody: 64 << 10},
	})
	if !strings.Contains(got, "maxRequestBodyBytes: 65536") {
		t.Errorf("missing the cap:\n%s", got)
	}
	if !strings.Contains(got, "memRequestBodyBytes: 65536") {
		t.Errorf("a sub-1MiB cap must lower memRequestBodyBytes:\n%s", got)
	}
}

func TestRenderRoutes_NoMaxBodyNoBuffering(t *testing.T) {
	got := renderRoutes("production", "core", []route{
		{Host: "api.example.com", Backends: []string{"127.0.0.1:40001"}, TLS: true},
	})
	if strings.Contains(got, "buffering") {
		t.Errorf("route without max_body must emit no buffering middleware:\n%s", got)
	}
}

func TestRenderRoutes_Passthrough(t *testing.T) {
	got := renderRoutes("front", "chat-production/sklp-l7-relay", []route{
		{Host: "meet.chat.eco", Backends: []string{"10.90.0.9:80"}, TLS: true},
		{Host: "turn.chat.eco", Backends: []string{"10.90.0.9:5349"}, Passthrough: true},
	})
	for _, sub := range []string{
		"http:\n",
		"Host(`meet.chat.eco`)",
		"tcp:\n  routers:\n",
		"rule: \"HostSNI(`turn.chat.eco`)\"",
		"passthrough: true",
		`address: "10.90.0.9:5349"`,
		"inFlightConn:\n        amount: 100",
	} {
		if !strings.Contains(got, sub) {
			t.Errorf("missing %q\n%s", sub, got)
		}
	}
	if strings.Contains(got, "http://10.90.0.9:5349") {
		t.Errorf("passthrough backend rendered as an HTTP server:\n%s", got)
	}

	only := renderRoutes("front", "x", []route{{Host: "turn.chat.eco", Backends: []string{"10.90.0.9:5349"}, Passthrough: true}})
	if strings.Contains(only, "http:") {
		t.Errorf("passthrough-only file has an http section:\n%s", only)
	}
}
