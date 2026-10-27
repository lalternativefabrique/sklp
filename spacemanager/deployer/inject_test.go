package deployer

import (
	"testing"

	"github.com/lalternativefabrique/sklp/spacemanager"
	// Blank-import the traefik driver so OpenGateway("traefik") resolves in the
	// tests (the driver registers itself via init; the binary blank-imports it at
	// the CLI edge, but a unit test must pull it in explicitly).
	_ "github.com/lalternativefabrique/sklp/spacemanager/gateway/traefik"
)

func TestRoutesToDomains_FillsPortFromService(t *testing.T) {
	services := []spacemanager.Service{
		{Name: "nginx", Port: 80},
		{Name: "api", Port: 8080},
	}
	routes := []spacemanager.ResolvedRoute{
		{Service: "nginx", Host: "nginx.test"},
		{Service: "api", Host: "api.test", Path: "/v1"},
	}
	got := routesToDomains("production", services, routes)
	if len(got) != 2 {
		t.Fatalf("got %d domains, want 2", len(got))
	}
	byHost := map[string]spacemanager.Domain{}
	for _, d := range got {
		byHost[d.Host] = d
	}
	if byHost["nginx.test"].Port != "80" {
		t.Errorf("nginx port = %q, want 80", byHost["nginx.test"].Port)
	}
	if byHost["api.test"].Port != "8080" {
		t.Errorf("api port = %q, want 8080", byHost["api.test"].Port)
	}
	if byHost["api.test"].Path != "/v1" {
		t.Errorf("api path = %q, want /v1", byHost["api.test"].Path)
	}
	if byHost["nginx.test"].Space != "production" {
		t.Errorf("space = %q, want production", byHost["nginx.test"].Space)
	}
}

func TestRoutesToDomains_SkipsUnknownService(t *testing.T) {
	services := []spacemanager.Service{{Name: "nginx", Port: 80}}
	routes := []spacemanager.ResolvedRoute{
		{Service: "nginx", Host: "nginx.test"},
		{Service: "ghost", Host: "ghost.test"}, // no matching service
	}
	got := routesToDomains("production", services, routes)
	if len(got) != 1 {
		t.Fatalf("got %d domains, want 1 (ghost skipped)", len(got))
	}
	if got[0].Service != "nginx" {
		t.Errorf("kept wrong service %q", got[0].Service)
	}
}

func TestInjectIngress_NoRoutes_LeavesServicesUnchanged(t *testing.T) {
	services := []spacemanager.Service{{Name: "nginx", Port: 80}}
	meta := spacemanager.SpaceMeta{Name: "production"} // no Routes
	got, err := injectIngress("traefik", services, meta)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("got %d services, want 1 (no injection without routes)", len(got))
	}
}

func TestInjectIngressContainer_ReservedNameCollision_Errors(t *testing.T) {
	// The retained container-injector path (explicit driver) must reject a user
	// service colliding with the injected ingress name.
	services := []spacemanager.Service{
		{Name: "nginx", Port: 80},
		{Name: "sklp-ingress", Port: 80}, // collides with the injected name
	}
	meta := spacemanager.SpaceMeta{
		Name:        "production",
		Routes:      []spacemanager.ResolvedRoute{{Service: "nginx", Host: "nginx.test", Path: "/api"}},
		GatewayMode: spacemanager.GatewayModePerSpace,
	}
	if _, err := injectIngressContainer("traefik", services, meta); err == nil {
		t.Fatal("expected an error on reserved-name collision, got nil")
	}
}

func TestInjectIngress_PerSpace_NoContainer_NativeL7(t *testing.T) {
	// Per-space mode uses the NATIVE in-worker L7 ingress, so injectIngress never
	// adds a container — even for a path route.
	services := []spacemanager.Service{{Name: "nginx", Port: 80}}
	meta := spacemanager.SpaceMeta{
		Name:        "production",
		Routes:      []spacemanager.ResolvedRoute{{Service: "nginx", Host: "app.test", Path: "/api"}},
		GatewayMode: spacemanager.GatewayModePerSpace,
	}
	got, err := injectIngress("traefik", services, meta)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("got %d services, want 1 (native L7, no injected container)", len(got))
	}
}

func TestInjectIngress_SingletonMode_LeavesServicesUnchanged(t *testing.T) {
	// Default (singleton) mode must NOT inject an ingress even with routes — the
	// legacy host-singleton path owns the front door there.
	services := []spacemanager.Service{{Name: "nginx", Port: 80}}
	meta := spacemanager.SpaceMeta{
		Name:   "production",
		Routes: []spacemanager.ResolvedRoute{{Service: "nginx", Host: "nginx.test"}},
		// GatewayMode empty == singleton
	}
	got, err := injectIngress("traefik", services, meta)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("got %d services, want 1 (no injection in singleton mode)", len(got))
	}
}

func TestInjectIngress_NonInjectorDriver_LeavesServicesUnchanged(t *testing.T) {
	// "fake" is the in-package driver that does NOT implement IngressInjector:
	// the legacy host-singleton path must be untouched. A PATH route is used so
	// the needsL7Ingress gate passes and we actually exercise the driver check.
	services := []spacemanager.Service{{Name: "nginx", Port: 80}}
	meta := spacemanager.SpaceMeta{
		Name:        "production",
		Routes:      []spacemanager.ResolvedRoute{{Service: "nginx", Host: "nginx.test", Path: "/api"}},
		GatewayMode: spacemanager.GatewayModePerSpace,
	}
	got, err := injectIngress("fake", services, meta)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("got %d services, want 1 (fake driver is not an injector)", len(got))
	}
}

func TestInjectIngress_HostOnlyRoute_NoIngress(t *testing.T) {
	// Option 4: a plain Host route is served by the mux dialing the service
	// directly, so NO ingress container is injected (the trivial case pays no
	// traefik). Only a Path (or future TLS) route triggers injection.
	services := []spacemanager.Service{{Name: "nginx", Port: 80}}
	meta := spacemanager.SpaceMeta{
		Name:        "production",
		Routes:      []spacemanager.ResolvedRoute{{Service: "nginx", Host: "nginx.test"}}, // no path
		GatewayMode: spacemanager.GatewayModePerSpace,
	}
	got, err := injectIngress("traefik", services, meta)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("got %d services, want 1 (Host-only route needs no ingress)", len(got))
	}
}

func TestInjectIngressContainer_PathRoute_InjectsIngress(t *testing.T) {
	// The retained container path injects a real ingress for a Path route.
	services := []spacemanager.Service{{Name: "nginx", Port: 80}}
	meta := spacemanager.SpaceMeta{
		Name:        "production",
		Routes:      []spacemanager.ResolvedRoute{{Service: "nginx", Host: "app.test", Path: "/api"}},
		GatewayMode: spacemanager.GatewayModePerSpace,
	}
	got, err := injectIngressContainer("traefik", services, meta)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d services, want 2 (nginx + injected ingress)", len(got))
	}
	if got[len(got)-1].Name != spacemanager.IngressServiceName {
		t.Errorf("injected service = %q, want %q", got[len(got)-1].Name, spacemanager.IngressServiceName)
	}
}
