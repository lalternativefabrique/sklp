package traefik

import (
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/spacemanager"
)

func TestIngressService_EmptyRoutes_DeclinesInjection(t *testing.T) {
	g := &traefikGateway{}
	_, _, _, ok := g.IngressService("production", nil)
	if ok {
		t.Fatal("expected ok=false for empty routes")
	}
}

func TestIngressService_BuildsServiceAndFiles(t *testing.T) {
	g := &traefikGateway{}
	routes := []spacemanager.Domain{
		{Host: "nginx.test", Service: "nginx", Port: "80"},
	}
	svc, files, mount, ok := g.IngressService("production", routes)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if svc.Name != ingressServiceName {
		t.Errorf("svc.Name = %q, want %q", svc.Name, ingressServiceName)
	}
	if svc.Image != imageTag {
		t.Errorf("svc.Image = %q, want %q", svc.Image, imageTag)
	}
	if svc.Port != ingressPort {
		t.Errorf("svc.Port = %d, want %d", svc.Port, ingressPort)
	}
	if mount != ingressCfgMount {
		t.Errorf("mount = %q, want %q", mount, ingressCfgMount)
	}
	if _, hasStatic := files["traefik.yaml"]; !hasStatic {
		t.Error("missing traefik.yaml")
	}
	if _, hasDyn := files["dynamic/routes.yaml"]; !hasDyn {
		t.Error("missing dynamic/routes.yaml")
	}
}

func TestRenderIngressRoutes_BackendIsDNSName(t *testing.T) {
	routes := []spacemanager.Domain{
		{Host: "nginx.test", Service: "nginx", Port: "80"},
	}
	out := renderIngressRoutes(routes)
	// Backend MUST be the DNS name nginx:80, not a loopback addr — the whole
	// point of the per-space ingress (resolved by space DNS inside the netns).
	if !strings.Contains(out, `url: "http://nginx:80"`) {
		t.Errorf("backend is not the DNS name nginx:80:\n%s", out)
	}
	if strings.Contains(out, "127.0.0.1") {
		t.Errorf("backend leaked a loopback addr:\n%s", out)
	}
	if !strings.Contains(out, "Host(`nginx.test`)") {
		t.Errorf("missing Host rule:\n%s", out)
	}
}

func TestRenderIngressRoutes_ExplicitBackendWins(t *testing.T) {
	routes := []spacemanager.Domain{
		{Host: "a.test", Service: "svc", Port: "80", Backends: []string{"svc:9000"}},
	}
	out := renderIngressRoutes(routes)
	if !strings.Contains(out, `url: "http://svc:9000"`) {
		t.Errorf("explicit backend not used:\n%s", out)
	}
}

func TestRenderIngressRoutes_Deterministic(t *testing.T) {
	routes := []spacemanager.Domain{
		{Host: "b.test", Service: "b", Port: "80"},
		{Host: "a.test", Service: "a", Port: "80"},
	}
	first := renderIngressRoutes(routes)
	// Reversed input must produce an identical file (sorted by host).
	rev := []spacemanager.Domain{routes[1], routes[0]}
	if second := renderIngressRoutes(rev); first != second {
		t.Errorf("non-deterministic output:\n%s\n---\n%s", first, second)
	}
	// a.test must come before b.test.
	if strings.Index(first, "a.test") > strings.Index(first, "b.test") {
		t.Errorf("routes not sorted by host:\n%s", first)
	}
}

func TestRenderIngressStatic_EntrypointOnIngressPort(t *testing.T) {
	out := renderIngressStatic()
	if !strings.Contains(out, ":8000") {
		t.Errorf("static config missing :8000 entrypoint:\n%s", out)
	}
	if !strings.Contains(out, "/etc/traefik/dynamic") {
		t.Errorf("static config missing file provider dir:\n%s", out)
	}
}
