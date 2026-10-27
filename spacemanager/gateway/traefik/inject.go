package traefik

import (
	"fmt"
	"sort"
	"strings"

	"github.com/lalternativefabrique/sklp/spacemanager"
)

// inject.go implements spacemanager.IngressInjector for the traefik driver: it
// materialises a PER-SPACE ingress container (running INSIDE the space netns)
// instead of the host singleton. The container resolves its backends by SPACE
// DNS NAME (service:port via the worker nameTable), so it dials containers
// directly on the bridge — no host loopback port per service.
//
// This is ADDITIVE: the singleton path (EnsureUp/Route/render.go) is untouched.
// The deployer type-asserts the driver to IngressInjector and injects the
// returned service only for spaces that declare routes.

// ingressServiceName / ingressPort are the shared core contract (a user service
// may not take the name; the mux targets it by name:port). Aliased locally so
// the renderers below read naturally.
const (
	ingressServiceName = spacemanager.IngressServiceName
	ingressPort        = spacemanager.IngressPort

	// ingressCfgMount is where the per-space ingress config dir is bind-mounted
	// inside the container. Holds traefik.yaml + dynamic/routes.yaml.
	ingressCfgMount = "/etc/traefik"
)

// IngressService returns the ingress Service to inject into space plus the config
// files to drop on disk for its bind mount. routes are this space's resolved
// public routes (as Domain values, Backends already set to DNS names). ok=false
// when there is nothing to route (the deployer then injects nothing).
func (g *traefikGateway) IngressService(space string, routes []spacemanager.Domain) (spacemanager.Service, map[string][]byte, string, bool) {
	if len(routes) == 0 {
		return spacemanager.Service{}, nil, "", false
	}

	files := map[string][]byte{
		"traefik.yaml":        []byte(renderIngressStatic()),
		"dynamic/routes.yaml": []byte(renderIngressRoutes(routes)),
	}

	// The deployer resolves the host config dir and builds the Volumes entry from
	// the returned mount target; we declare only the in-container path + the spec.
	svc := spacemanager.Service{
		Name:    ingressServiceName,
		Image:   imageTag,
		Command: []string{"--configFile=" + ingressCfgMount + "/traefik.yaml"},
		Port:    ingressPort,
	}
	return svc, files, ingressCfgMount, true
}

// renderIngressStatic is the traefik STATIC config for an in-netns ingress: one
// plain HTTP entrypoint on ingressPort, a file provider watching the mounted
// dynamic dir. No ACME here — TLS termination moves to the host mux (PR2); this
// ingress speaks plain HTTP to the mux over the fd-passed connection.
func renderIngressStatic() string {
	var b strings.Builder
	b.WriteString("global:\n")
	b.WriteString("  checkNewVersion: false\n")
	b.WriteString("  sendAnonymousUsage: false\n")
	b.WriteString("api:\n")
	b.WriteString("  dashboard: false\n")
	b.WriteString("entryPoints:\n")
	fmt.Fprintf(&b, "  %s:\n    address: \":%d\"\n", entryWeb, ingressPort)
	b.WriteString("providers:\n")
	b.WriteString("  file:\n")
	fmt.Fprintf(&b, "    directory: %q\n", ingressCfgMount+"/dynamic")
	b.WriteString("    watch: true\n")
	return b.String()
}

// renderIngressRoutes renders ONE dynamic file holding every route of the space.
// Each Domain becomes a router (Host rule) and a service whose single backend is
// the DNS name service:port — resolved by the space DNS from inside the netns.
// Deterministic ordering so an identical route set produces an identical file
// (no spurious file-provider reload).
func renderIngressRoutes(routes []spacemanager.Domain) string {
	sorted := append([]spacemanager.Domain(nil), routes...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Host != sorted[j].Host {
			return sorted[i].Host < sorted[j].Host
		}
		return sorted[i].Path < sorted[j].Path
	})

	var routers, services strings.Builder
	for i, d := range sorted {
		name := fmt.Sprintf("%s-%d", sanitizeFile(d.Service), i)
		rule := fmt.Sprintf("Host(`%s`)", d.Host)
		if d.Path != "" {
			rule += fmt.Sprintf(" && PathPrefix(`%s`)", d.Path)
		}
		fmt.Fprintf(&routers, "    %s:\n", name)
		fmt.Fprintf(&routers, "      rule: \"%s\"\n", rule)
		fmt.Fprintf(&routers, "      service: %s\n", name)
		fmt.Fprintf(&routers, "      entryPoints: [%s]\n", entryWeb)

		// Backend = the service's DNS name:port (resolved by space DNS). Falls
		// back to the declared Service name when no explicit backend is set.
		backend := fmt.Sprintf("%s:%s", d.Service, d.Port)
		if len(d.Backends) > 0 {
			backend = d.Backends[0]
		}
		fmt.Fprintf(&services, "    %s:\n      loadBalancer:\n        servers:\n", name)
		fmt.Fprintf(&services, "          - url: \"http://%s\"\n", backend)
	}

	var b strings.Builder
	b.WriteString("http:\n  routers:\n")
	b.WriteString(routers.String())
	b.WriteString("  services:\n")
	b.WriteString(services.String())
	return b.String()
}
