package spacemanager

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// proxyController is the injected reverse-proxy front-end, mirroring the
// Launcher/readyChecker pattern: the production impl writes a traefik dynamic
// config (file provider, hot-reloaded) and lets traefik load-balance; tests
// inject a fake that just records SetBackends calls, so the proxy/rolling
// behaviour is asserted without a real traefik, network, or privilege.
//
// SetBackends declares the CURRENT set of Ready backend addresses for a
// service. The agent calls it whenever that set changes: an instance becoming
// Ready adds its address; an instance leaving Ready (death, rolling cutover,
// teardown) is removed BEFORE it is killed, so the proxy never routes to a
// gone instance. addrs is the complete desired set (not a delta); an empty
// slice means "no Ready backend" (the route is left with nothing to serve
// rather than serving a dead instance).
type proxyController interface {
	SetBackends(service string, addrs []string) error
}

// traefikProxy is the production proxyController. It owns one dynamic-config
// file per service under dynDir; traefik's file provider watches the directory
// and hot-reloads on change (no proxy restart, no dropped connections). The
// proxy itself runs as an ordinary managed service (the agent launches the
// traefik container like any other); this type only maintains its config.
//
// NOT exercised by tests (writing real traefik YAML needs a running traefik to
// be meaningful, and CI has no network); the fake covers the agent contract.
type traefikProxy struct {
	mu     sync.Mutex
	dynDir string
	routes map[string]Service // service -> its routing spec (Port/Proxy)
}

// NewTraefikProxy builds the production file-provider proxy controller: it
// writes per-service traefik dynamic config under dynDir (traefik's file
// provider hot-reloads on change) and pre-registers the routing spec of every
// proxied service in desired. It is the prod counterpart to NewRuncLauncher;
// tests inject a fake proxyController instead. NOTE: the actual traefik config
// emitted here is NOT exercised by tests (it needs a running traefik + network,
// absent in CI) — the fake covers the agent's SetBackends contract.
func NewTraefikProxy(dynDir string, desired []Service) (proxyController, error) {
	p, err := newTraefikProxy(dynDir)
	if err != nil {
		return nil, err
	}
	for _, svc := range desired {
		if svc.proxied() {
			p.register(svc)
		}
	}
	return p, nil
}

// newTraefikProxy builds a file-provider proxy controller writing into dynDir.
func newTraefikProxy(dynDir string) (*traefikProxy, error) {
	if err := os.MkdirAll(dynDir, 0o755); err != nil {
		return nil, fmt.Errorf("traefik proxy: dyn dir: %w", err)
	}
	return &traefikProxy{dynDir: dynDir, routes: map[string]Service{}}, nil
}

// register records the routing spec (Port, optional Proxy rule) for a
// service so SetBackends can render a full router+service block. Called once
// per proxied service before the first SetBackends.
func (p *traefikProxy) register(svc Service) {
	p.mu.Lock()
	p.routes[svc.Name] = svc
	p.mu.Unlock()
}

func (p *traefikProxy) SetBackends(service string, addrs []string) error {
	p.mu.Lock()
	svc := p.routes[service]
	p.mu.Unlock()

	// Deterministic order so the file is stable across identical sets (avoids
	// spurious reloads when nothing actually changed).
	sorted := append([]string(nil), addrs...)
	sort.Strings(sorted)

	var b strings.Builder
	b.WriteString("http:\n  routers:\n")
	rule := svc.Proxy
	if rule == "" {
		rule = "PathPrefix(`/" + service + "`)"
	} else if !strings.ContainsAny(rule, "(`") {
		// A bare host/path was given; wrap it as a host rule by default.
		rule = "Host(`" + rule + "`)"
	}
	fmt.Fprintf(&b, "    %s:\n      rule: %q\n      service: %s\n", service, rule, service)
	b.WriteString("  services:\n")
	fmt.Fprintf(&b, "    %s:\n      loadBalancer:\n        servers:\n", service)
	for _, a := range sorted {
		fmt.Fprintf(&b, "          - url: %q\n", backendURL(a, svc.Port))
	}

	path := filepath.Join(p.dynDir, sanitizeFile(service)+".yaml")
	// Atomic-ish replace: write a temp file then rename so traefik never reads
	// a half-written config.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("traefik proxy: write %s: %w", service, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("traefik proxy: swap %s: %w", service, err)
	}
	return nil
}

// backendURL builds the http URL traefik dials for an instance address and the
// service's application port.
func backendURL(addr string, port int) string {
	if port == 0 {
		return "http://" + addr
	}
	return "http://" + addr + ":" + strconv.Itoa(port)
}

// sanitizeFile makes a service name safe as a file stem.
func sanitizeFile(s string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, s)
}
