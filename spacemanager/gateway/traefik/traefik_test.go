package traefik

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/spacemanager"
)

// newDriver builds a traefik driver against a temp $XDG_CONFIG_HOME and returns
// the concrete type for FS assertions.
func newDriver(t *testing.T) *traefikGateway {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	gw, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tg, ok := gw.(*traefikGateway)
	if !ok {
		t.Fatalf("New() = %T, want *traefikGateway", gw)
	}
	return tg
}

// TestRoute_EmitsTraefikRouter: a TLS domain writes a namespaced router with a
// Host rule, the websecure entrypoint, the cert resolver, and one server url per
// backend.
func TestRoute_EmitsTraefikRouter(t *testing.T) {
	tg := newDriver(t)
	d := spacemanager.Domain{
		Host:     "api.example.com",
		Space:    "production",
		Service:  "core",
		Port:     "4000",
		TLS:      true,
		Backends: []string{"127.0.0.1:40001", "127.0.0.1:40002"},
	}
	if err := tg.Route("production", "core", []spacemanager.Domain{d}); err != nil {
		t.Fatalf("Route: %v", err)
	}

	want := filepath.Join(spacemanager.GatewayDir(), "dynamic", "production-core.yaml")
	data, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("expected namespaced route file %s: %v", want, err)
	}
	s := string(data)
	for _, sub := range []string{
		"Host(`api.example.com`)",
		"entryPoints: [websecure]",
		"certResolver: le",
		`url: "http://127.0.0.1:40001"`,
		`url: "http://127.0.0.1:40002"`,
	} {
		if !strings.Contains(s, sub) {
			t.Errorf("router missing %q\n---\n%s", sub, s)
		}
	}
}

// TestRoute_NoTLSUsesWebEntrypoint: a non-TLS domain routes on the web entrypoint
// and omits the tls block entirely.
func TestRoute_NoTLSUsesWebEntrypoint(t *testing.T) {
	tg := newDriver(t)
	d := spacemanager.Domain{Host: "app.local", Space: "dev", Service: "web", TLS: false, Backends: []string{"127.0.0.1:5000"}}
	if err := tg.Route("dev", "web", []spacemanager.Domain{d}); err != nil {
		t.Fatalf("Route: %v", err)
	}
	data, err := os.ReadFile(tg.routeFilePath("dev", "web"))
	if err != nil {
		t.Fatalf("read route: %v", err)
	}
	s := string(data)
	if !strings.Contains(s, "entryPoints: [web]") {
		t.Errorf("non-TLS route not on web entrypoint:\n%s", s)
	}
	if strings.Contains(s, "tls:") || strings.Contains(s, "certResolver") {
		t.Errorf("non-TLS route must omit the tls block:\n%s", s)
	}
}

// TestRoute_EmptyBackends writes a router with no servers, so a re-Route on
// backend arrival just rewrites the file.
func TestRoute_EmptyBackends(t *testing.T) {
	tg := newDriver(t)
	d := spacemanager.Domain{Host: "api.example.com", Space: "production", Service: "core", TLS: true}
	if err := tg.Route("production", "core", []spacemanager.Domain{d}); err != nil {
		t.Fatalf("Route: %v", err)
	}
	data, err := os.ReadFile(tg.routeFilePath("production", "core"))
	if err != nil {
		t.Fatalf("read route: %v", err)
	}
	s := string(data)
	if !strings.Contains(s, "Host(`api.example.com`)") {
		t.Errorf("router missing host rule:\n%s", s)
	}
	if strings.Contains(s, "url:") {
		t.Errorf("empty-backend router must carry no server url:\n%s", s)
	}

	// Re-Route with backends just rewrites the same file.
	d.Backends = []string{"127.0.0.1:40001"}
	if err := tg.Route("production", "core", []spacemanager.Domain{d}); err != nil {
		t.Fatalf("re-Route: %v", err)
	}
	data, _ = os.ReadFile(tg.routeFilePath("production", "core"))
	if !strings.Contains(string(data), `url: "http://127.0.0.1:40001"`) {
		t.Errorf("re-Route did not add the backend:\n%s", data)
	}
}

// TestUnroute_RemovesFile: Unroute(space, service) removes the namespaced file
// declaring that unit's routes, and is idempotent for an absent unit / missing
// dyn dir.
func TestUnroute_RemovesFile(t *testing.T) {
	tg := newDriver(t)
	d := spacemanager.Domain{Host: "api.example.com", Space: "production", Service: "core", TLS: true}
	if err := tg.Route("production", "core", []spacemanager.Domain{d}); err != nil {
		t.Fatalf("Route: %v", err)
	}
	file := tg.routeFilePath("production", "core")
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("route file not written: %v", err)
	}
	if err := tg.Unroute("production", "core"); err != nil {
		t.Fatalf("Unroute: %v", err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("route file still present after Unroute (err=%v)", err)
	}
	// Idempotent: absent unit, and an empty dyn dir.
	if err := tg.Unroute("production", "gone"); err != nil {
		t.Fatalf("Unroute(absent): %v", err)
	}
}

// TestPruneRoutes_RemovesOrphansKeepsWanted: PruneRoutes drops route files whose
// (space, service) is not in keep, keeps the wanted ones, sweeps a stale .tmp,
// and never touches a foreign non-.yaml file. This is the SIGKILL self-heal: a
// route left behind by a supervisor that died before Unroute is reconciled away.
func TestPruneRoutes_RemovesOrphansKeepsWanted(t *testing.T) {
	tg := newDriver(t)
	r := func(space, svc, host string) {
		if err := tg.Route(space, svc, []spacemanager.Domain{{Host: host, Space: space, Service: svc, TLS: true}}); err != nil {
			t.Fatalf("Route %s/%s: %v", space, svc, err)
		}
	}
	r("production", "core", "a.example.com") // kept
	r("production", "web", "b.example.com")  // orphan (no live owner)
	r("staging", "core", "c.example.com")    // kept

	// A leftover atomic-write temp file and a foreign file the gateway does not own.
	if err := os.WriteFile(filepath.Join(tg.dynDir, "production-core.yaml.tmp"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(tg.dynDir, "operator-notes.txt")
	if err := os.WriteFile(foreign, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}

	keep := map[spacemanager.RouteKey]bool{
		{Space: "production", Service: "core"}: true,
		{Space: "staging", Service: "core"}:    true,
	}
	if err := tg.PruneRoutes(keep); err != nil {
		t.Fatalf("PruneRoutes: %v", err)
	}

	mustExist := []string{"production-core.yaml", "staging-core.yaml", "operator-notes.txt"}
	for _, n := range mustExist {
		if _, err := os.Stat(filepath.Join(tg.dynDir, n)); err != nil {
			t.Errorf("PruneRoutes removed a file it should keep: %s (%v)", n, err)
		}
	}
	mustGone := []string{"production-web.yaml", "production-core.yaml.tmp"}
	for _, n := range mustGone {
		if _, err := os.Stat(filepath.Join(tg.dynDir, n)); !os.IsNotExist(err) {
			t.Errorf("PruneRoutes left an orphan: %s (err=%v)", n, err)
		}
	}
}

// TestPruneRoutes_EmptyKeepPrunesAll: an empty keep set removes every owned route
// file (the host has no live space) but is otherwise safe.
func TestPruneRoutes_EmptyKeepPrunesAll(t *testing.T) {
	tg := newDriver(t)
	if err := tg.Route("dev", "web", []spacemanager.Domain{{Host: "x.local", Space: "dev", Service: "web"}}); err != nil {
		t.Fatalf("Route: %v", err)
	}
	if err := tg.PruneRoutes(nil); err != nil {
		t.Fatalf("PruneRoutes(nil): %v", err)
	}
	if _, err := os.Stat(tg.routeFilePath("dev", "web")); !os.IsNotExist(err) {
		t.Errorf("empty keep set did not prune the route (err=%v)", err)
	}
}

// TestPruneRoutes_NoDynDir: pruning before any route was ever written (no dyn
// dir) is a no-op, not an error.
func TestPruneRoutes_NoDynDir(t *testing.T) {
	tg := newDriver(t)
	if err := tg.PruneRoutes(map[spacemanager.RouteKey]bool{{Space: "a", Service: "b"}: true}); err != nil {
		t.Fatalf("PruneRoutes with no dyn dir: %v", err)
	}
}

// TestNamespacedFilename: two spaces routing the same service get distinct files.
func TestNamespacedFilename(t *testing.T) {
	tg := newDriver(t)
	if err := tg.Route("production", "core", []spacemanager.Domain{{Host: "a.example.com", Space: "production", Service: "core", TLS: true}}); err != nil {
		t.Fatalf("Route prod: %v", err)
	}
	if err := tg.Route("staging", "core", []spacemanager.Domain{{Host: "b.example.com", Space: "staging", Service: "core", TLS: true}}); err != nil {
		t.Fatalf("Route staging: %v", err)
	}
	for _, name := range []string{"production-core.yaml", "staging-core.yaml"} {
		if _, err := os.Stat(filepath.Join(tg.dynDir, name)); err != nil {
			t.Errorf("expected namespaced file %s: %v", name, err)
		}
	}
}

// TestRegistry_OpenGatewayTraefik: importing this package registers the driver,
// so spacemanager.OpenGateway("traefik") constructs the real traefik driver.
func TestRegistry_OpenGatewayTraefik(t *testing.T) {
	gw, err := spacemanager.OpenGateway("traefik")
	if err != nil {
		t.Fatalf("OpenGateway(traefik): %v", err)
	}
	if _, ok := gw.(*traefikGateway); !ok {
		t.Fatalf("OpenGateway(traefik) = %T, want *traefikGateway", gw)
	}
	// Empty name defaults to traefik too, now that its constructor is linked.
	gw, err = spacemanager.OpenGateway("")
	if err != nil {
		t.Fatalf("OpenGateway(\"\"): %v", err)
	}
	if _, ok := gw.(*traefikGateway); !ok {
		t.Fatalf("OpenGateway(\"\") = %T, want default *traefikGateway", gw)
	}
}

// TestRoute_CarriesRateLimit: a Domain's declared rate reaches the rendered
// file. The limit used to stop at the neutral model — declared in deploy.yaml,
// enforced by the native L7, silently dropped by this driver.
func TestRoute_CarriesRateLimit(t *testing.T) {
	tg := newDriver(t)
	d := spacemanager.Domain{
		Host: "api.example.com", Space: "production", Service: "core", TLS: true,
		Backends: []string{"127.0.0.1:40001"}, Rate: 10, Burst: 20,
	}
	if err := tg.Route("production", "core", []spacemanager.Domain{d}); err != nil {
		t.Fatalf("Route: %v", err)
	}
	data, err := os.ReadFile(tg.routeFilePath("production", "core"))
	if err != nil {
		t.Fatalf("read route: %v", err)
	}
	s := string(data)
	for _, sub := range []string{"rateLimit:", "average: 10", "burst: 20"} {
		if !strings.Contains(s, sub) {
			t.Errorf("routed file missing %q\n---\n%s", sub, s)
		}
	}
}
