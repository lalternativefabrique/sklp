package deployer

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/lalternativefabrique/sklp/spacemanager"
)

// inject.go injects a PER-SPACE ingress service when the gateway driver supports
// it (spacemanager.IngressInjector) and the space declares routes. It is the
// deploy-time counterpart to the driver's IngressService: the driver decides
// WHAT to run + WHICH config files; the deployer owns WHERE on disk the config
// lives and wires the bind mount. A driver that is not an IngressInjector (or a
// space with no routes) leaves the service set untouched — the legacy
// host-singleton path is unaffected (no degradation).

// injectIngress appends the per-space ingress service to services when driverName
// supports per-space injection and meta declares routes. It writes the driver's
// config files under the per-space ingress dir and binds that dir into the
// container. Returns the (possibly unchanged) service set.
func injectIngress(driverName string, services []spacemanager.Service, meta spacemanager.SpaceMeta) ([]spacemanager.Service, error) {
	// Only inject in per-space mode: the singleton path (default) keeps the host
	// traefik + loopback ingress, so injecting a second ingress there would be
	// redundant and fight the singleton. No routes = nothing to front.
	if meta.GatewayMode != spacemanager.GatewayModePerSpace || len(meta.Routes) == 0 {
		return services, nil
	}

	// L7 routing in per-space mode is served by the NATIVE in-worker ingress
	// (sklp-l7, a worker goroutine pushed via SetL7Routes) — no container, no
	// image, no traefik. So a per-space deploy injects NO ingress container: the
	// native L7 handles Host + path routing. The IngressInjector path (below) is
	// retained for a future explicit container driver (caddy/traefik for TLS +
	// rich middlewares) but is not the default. Native is the default front door.
	_ = driverName
	return services, nil
}

// injectIngressContainer is the retained IngressInjector path for an explicit
// container driver. It is currently unused (per-space uses the native L7), kept
// so a future `gateway.driver: caddy` can opt back into a real ingress container
// for TLS/middlewares. Same logic as the original injectIngress body.
func injectIngressContainer(driverName string, services []spacemanager.Service, meta spacemanager.SpaceMeta) ([]spacemanager.Service, error) {
	if !needsL7Ingress(meta.Routes) {
		return services, nil
	}

	drv, err := spacemanager.OpenGateway(driverName)
	if err != nil {
		return nil, fmt.Errorf("ingress: open gateway driver %q: %w", driverName, err)
	}
	inj, ok := drv.(spacemanager.IngressInjector)
	if !ok {
		// Driver runs as a host singleton, not a per-space container: keep the
		// legacy path, inject nothing.
		return services, nil
	}

	domains := routesToDomains(meta.Name, services, meta.Routes)
	svc, files, mount, ok := inj.IngressService(meta.Name, domains)
	if !ok {
		return services, nil
	}

	// Guard against a user service colliding with the injected ingress name: the
	// injected service would otherwise duplicate or shadow it. Surface it rather
	// than silently overwrite (a future change can reserve the name at parse).
	for _, s := range services {
		if s.Name == svc.Name {
			return nil, fmt.Errorf("ingress: service name %q is reserved for the injected per-space ingress", svc.Name)
		}
	}

	cfgDir := ingressConfigDir(meta.Name)
	if err := writeIngressFiles(cfgDir, files); err != nil {
		return nil, err
	}

	// Bind the host config dir into the container at the driver's mount target.
	svc.Volumes = append([]string{cfgDir + ":" + mount}, svc.Volumes...)

	return append(services, svc), nil
}

// needsL7Ingress reports whether any route requires the in-space L7 ingress
// rather than the mux's direct Host->service routing. Today that's a Path route
// (host shared across paths — the mux can't split it). The SNI/TLS step will add
// TLS routes here. A set of plain Host routes needs no ingress at all.
func needsL7Ingress(routes []spacemanager.ResolvedRoute) bool {
	for _, r := range routes {
		if r.Path != "" {
			return true
		}
	}
	return false
}

// routesToDomains maps the space's resolved routes to gateway Domains, filling
// each Domain.Port from the matching service's declared Port so the driver can
// build a DNS-name backend (service:port). Routes whose service is absent from
// the set are skipped (a config-resolution invariant normally prevents this).
func routesToDomains(space string, services []spacemanager.Service, routes []spacemanager.ResolvedRoute) []spacemanager.Domain {
	portOf := make(map[string]int, len(services))
	for _, s := range services {
		portOf[s.Name] = s.Port
	}
	out := make([]spacemanager.Domain, 0, len(routes))
	for _, r := range routes {
		port, ok := portOf[r.Service]
		if !ok {
			continue
		}
		out = append(out, spacemanager.Domain{
			Host:    r.Host,
			Path:    r.Path,
			Space:   space,
			Service: r.Service,
			Port:    strconv.Itoa(port),
			TLS:     false, // TLS termination moves to the host mux (PR2)
		})
	}
	return out
}

// ingressConfigDir is the host dir holding one space's ingress config (the
// driver's rendered files), bind-mounted into the ingress container. Namespaced
// per space under the gateway config root.
func ingressConfigDir(space string) string {
	return filepath.Join(spacemanager.GatewayConfigRoot(), "sklp", "spaces", space, "ingress")
}

// writeIngressFiles writes the driver's config files (relative path -> contents)
// under dir, creating parent dirs. Existing files are overwritten so a re-deploy
// reflects the latest routes (traefik's file provider hot-reloads on change).
func writeIngressFiles(dir string, files map[string][]byte) error {
	for rel, content := range files {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("ingress: mkdir %q: %w", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			return fmt.Errorf("ingress: write %q: %w", path, err)
		}
	}
	return nil
}
