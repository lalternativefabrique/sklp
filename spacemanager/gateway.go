package spacemanager

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Gateway is the host-level ingress front door: it routes an external
// hostname to a service of a space, terminating TLS. It is a SINGLETON for
// the host (owns :443/:80 + the cert store), shared across all spaces.
// sklp depends on this interface; traefik is one driver behind it.
//
// It is the multi-space sibling of proxyController: proxyController fronts one
// space's replicas (SetBackends), Gateway fronts the whole host by hostname.
// The deploy/agent core depends only on this interface — never on a traefik
// type — exactly like the proxy injection at the CLI edge.
type Gateway interface {
	// Route declares/replaces ALL routes a (space, service) is exposed on, as one
	// atomic unit (one vendor file per space/service). A service may carry several
	// routes (multiple hosts, or a host shared with siblings on distinct paths).
	// An empty routes slice is equivalent to Unroute(space, service).
	Route(space, service string, routes []Domain) error
	// Unroute removes every route owned by (space, service).
	Unroute(space, service string) error
}

// Domain is one public route of a (space, service): a host, an optional path
// prefix, the target port, TLS, and the resolved backends. The deploy core
// builds these from spaces.<name>.gateway.routes and hands the per-service set
// to Gateway.Route. Space/Service identify the owning unit (the vendor file
// key); Host/Path identify the route within it.
type Domain struct {
	Host     string   // api.example.com
	Path     string   // optional path prefix, e.g. "/api"; "" = host root
	Space    string   // production
	Service  string   // core
	Backends []string // resolved internal addrs of Ready replicas (may be empty before ready)
	Port     string   // the service Port the gateway targets
	TLS      bool     // terminate TLS (ACME) for this host

	// Response behaviours carried from the neutral route model, so a driver
	// applies the same ones the native L7 does.
	RedirectApex    bool          // 301 www.<host> -> <host>
	SecurityHeaders bool          // HSTS, frameDeny, nosniff, referrer-policy
	Compress        bool          // gzip text responses
	Rate            float64       // per-client-IP requests/sec; 0 = unlimited
	Burst           int           // token-bucket size
	Timeout         time.Duration // per-request cap; 0 = unbounded
	MaxBody         int64         // per-request body cap in bytes; 0 = unbounded

	// BackendHost, when non-empty, is the Host header the driver must send
	// UPSTREAM instead of the public one. A mesh route's backend is the remote
	// node's Network Controller, which routes by internal name
	// ("<service>.<space>.internal") — without the rewrite the remote side
	// sees the public host and answers 404.
	BackendHost string

	// Passthrough splices the TLS stream by SNI to Backends, undecrypted.
	Passthrough bool
}

// GatewayStatus is the observable state of the host gateway singleton.
type GatewayStatus struct {
	Driver    string // selected driver name (e.g. "traefik")
	Running   bool   // the singleton is up (listeners/cert store ready)
	Listening string // human-readable listen description (e.g. ":443/:80")
}

// RouteKey identifies a (space, service) route the gateway owns — the unit of a
// vendor route file. PruneRoutes is driven by a set of these.
type RouteKey struct {
	Space   string
	Service string
}

// GatewayDriver is a concrete gateway implementation plus its lifecycle. A
// driver IS a Gateway (Route/Unroute) and additionally owns the singleton:
// EnsureUp/Down/Status. Lifecycle is a driver concern, NOT a per-call one, so it
// stays off the Gateway interface the deploy core depends on.
type GatewayDriver interface {
	Gateway
	EnsureUp() error // make the singleton ready (idempotent): :443/:80 listener, cert store, dyn dir
	Down() error     // stop the singleton
	Status() (GatewayStatus, error)
	// PruneRoutes drops every route file the driver owns whose (space, service)
	// is NOT in keep — the garbage-collection sweep that reconciles the on-disk
	// route set against the live desired set. It exists because a supervisor
	// SIGKILLed before its teardown Unroute runs leaves a stale route file
	// pointing at a dead backend; the controller calls this once at gateway-up
	// with the keys every live space declares, so a crashed host self-heals.
	// Idempotent and safe on an empty keep set (which prunes ALL route files).
	PruneRoutes(keep map[RouteKey]bool) error
}

// IngressInjector is an OPTIONAL capability a gateway driver may implement to run
// as a PER-SPACE ingress container (inside the space netns) instead of — or in
// addition to — a host singleton. A driver that implements it is asked, at deploy
// time, to materialise the ingress: the Service to inject into the space and the
// config files to drop on disk for that service's bind mount.
//
// The deployer type-asserts the driver to this interface: a driver that does NOT
// implement it keeps the legacy host-singleton behaviour untouched (no
// degradation). The injected ingress dials its backends by SPACE DNS NAME
// (service:port, resolved by the worker nameTable from inside the netns), never
// by host IP — so it needs no loopback port per service, and a future
// network-domain (one worker, N bridges) resolves the same names unchanged.
type IngressInjector interface {
	// IngressService describes the per-space ingress to inject. routes are the
	// space's resolved public routes (already filtered to this space). It returns
	// the Service to run, the config files (relative path -> contents) the deployer
	// writes under the per-space ingress dir, and Mount: the in-container path that
	// dir must be bind-mounted at. The deployer owns the host dir location and
	// builds the final Volumes entry "<hostDir>:<Mount>". ok=false declines
	// injection for this route set (e.g. empty routes).
	IngressService(space string, routes []Domain) (svc Service, files map[string][]byte, mount string, ok bool)
}

const (
	// IngressServiceName is the reserved name of the per-space ingress service a
	// driver injects. Shared contract between the driver (which builds the
	// Service), the deployer (which injects it), and the host mux (which targets
	// it by name when fd-passing). A user service may not take this name.
	IngressServiceName = "sklp-ingress"

	// IngressPort is the in-netns port the injected ingress listens on. The host
	// mux fd-passes external connections to IngressServiceName:IngressPort, which
	// the space worker dials in the netns. Unprivileged.
	IngressPort = 8000

	// L7ServiceName / L7Port address the NATIVE in-worker L7 ingress (a goroutine
	// of the space worker, not a container). The mux targets it for path routes;
	// it is always present in per-space mode (the worker registers it in DNS), so
	// it needs no injected container. Must match spacenet's l7ServiceName/l7Port.
	L7ServiceName = "sklp-l7"
	L7Port        = 8001

	// L7TLSServiceName / L7TLSPort address the native in-worker L7's HTTPS
	// endpoint, which TERMINATES TLS (self-signed by default). The mux SNI-routes
	// every HTTPS connection here; the in-netns ingress decrypts and re-routes by
	// Host/path. Must match spacenet's l7TLSServiceName/l7TLSPort.
	L7TLSServiceName = "sklp-l7-tls"
	L7TLSPort        = 8443
)

// GatewayConstructor builds a driver instance. The registry maps a driver name
// to its constructor so OpenGateway can select one by config name. It is
// exported so an out-of-package vendor driver (e.g. the traefik package) can
// register itself via RegisterGatewayDriver — the deploy core never imports a
// vendor driver, the driver imports deploy one-way.
type GatewayConstructor func() (GatewayDriver, error)

// gatewayDrivers is the driver registry: name -> constructor. It is the single
// extension point — a new front door (caddy, nginx, sklp-native) registers here
// via RegisterGatewayDriver (from its own package's init, pulled in by a blank
// import at the CLI edge) without the deploy/agent core importing it, like
// swapping a db.Executor. Only the in-package "fake" driver is seeded here.
var gatewayDrivers = map[string]GatewayConstructor{
	"fake": func() (GatewayDriver, error) { return newFakeGateway(), nil },
}

// RegisterGatewayDriver adds a gateway driver constructor under name. A vendor
// driver package calls it from an init() and is pulled into the binary by a
// blank import at the CLI edge — so package deploy depends on NO vendor driver
// code (the agnosticism invariant). A duplicate name panics (a build-time wiring
// bug, never a runtime input).
func RegisterGatewayDriver(name string, ctor GatewayConstructor) {
	if _, dup := gatewayDrivers[name]; dup {
		panic(fmt.Sprintf("deploy: gateway driver %q already registered", name))
	}
	gatewayDrivers[name] = ctor
}

// DefaultGatewayDriver is the driver selected when config omits gateway.driver.
// traefik is the documented default; its constructor is registered out-of-package
// (see the traefik driver package), so the NAME is always known to config
// validation even before that package's blank import has seeded the constructor.
const DefaultGatewayDriver = "traefik"

// KnownGatewayDriver reports whether name is a registered gateway driver. The
// config layer uses it to reject an unknown gateway.driver at parse time. The
// default driver name is always accepted so config validation does not depend on
// the vendor driver package being blank-imported into the validating binary.
func KnownGatewayDriver(name string) bool {
	if name == DefaultGatewayDriver {
		return true
	}
	_, ok := gatewayDrivers[name]
	return ok
}

// OpenGateway selects and constructs the gateway driver by name. An empty name
// defaults to DefaultGatewayDriver; an unknown name is a clear error listing the
// registered drivers. The returned driver is NOT yet up — call EnsureUp.
func OpenGateway(driverName string) (GatewayDriver, error) {
	if driverName == "" {
		driverName = DefaultGatewayDriver
	}
	ctor, ok := gatewayDrivers[driverName]
	if !ok {
		if driverName == DefaultGatewayDriver {
			// The name is valid but its constructor was not registered: the binary
			// was built without the driver's blank import (a CLI-edge wiring bug).
			return nil, fmt.Errorf("gateway driver %q is not linked into this binary (missing blank import of its package)", driverName)
		}
		return nil, fmt.Errorf("unknown gateway driver %q (registered: %s)", driverName, knownGatewayDriverList())
	}
	return ctor()
}

// knownGatewayDriverList renders the registered driver names, sorted, for error
// messages.
func knownGatewayDriverList() string {
	names := make([]string, 0, len(gatewayDrivers))
	for n := range gatewayDrivers {
		names = append(names, n)
	}
	sort.Strings(names)
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out
}

// fakeGateway is the in-memory test/contract driver: it records Route/Unroute
// and lifecycle calls instead of touching the host. It lets the deploy-side
// Route/Unroute contract be asserted without a running traefik, network, or
// privilege — the multi-space sibling of the fake proxyController.
type fakeGateway struct {
	mu       sync.Mutex
	up       bool
	routes   map[string][]Domain // "space/service" -> its last routed set
	routed   []Domain            // ordered Route history (flattened)
	unrouted []string            // ordered Unroute history ("space/service")
	ups      int
	downs    int
}

func newFakeGateway() *fakeGateway {
	return &fakeGateway{routes: map[string][]Domain{}}
}

func fakeRouteKey(space, service string) string { return space + "/" + service }

func (f *fakeGateway) Route(space, service string, routes []Domain) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(routes) == 0 {
		delete(f.routes, fakeRouteKey(space, service))
		f.unrouted = append(f.unrouted, fakeRouteKey(space, service))
		return nil
	}
	f.routes[fakeRouteKey(space, service)] = routes
	f.routed = append(f.routed, routes...)
	return nil
}

func (f *fakeGateway) Unroute(space, service string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.routes, fakeRouteKey(space, service))
	f.unrouted = append(f.unrouted, fakeRouteKey(space, service))
	return nil
}

func (f *fakeGateway) EnsureUp() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.up = true
	f.ups++
	return nil
}

func (f *fakeGateway) Down() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.up = false
	f.downs++
	return nil
}

func (f *fakeGateway) Status() (GatewayStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return GatewayStatus{Driver: "fake", Running: f.up}, nil
}

// PruneRoutes drops every recorded route whose (space, service) is not in keep,
// mirroring the real driver's dyn-dir sweep so the GC contract is asserted
// against the fake. Each pruned key is recorded in the unroute history.
func (f *fakeGateway) PruneRoutes(keep map[RouteKey]bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for key := range f.routes {
		space, service, _ := strings.Cut(key, "/")
		if keep[RouteKey{Space: space, Service: service}] {
			continue
		}
		delete(f.routes, key)
		f.unrouted = append(f.unrouted, key)
	}
	return nil
}

// GatewayConfigRoot resolves the XDG config base ($XDG_CONFIG_HOME, else
// ~/.config). It mirrors control.configRoot (unexported there) so the gateway's
// HOST-level dir honours the same env override tests rely on. Exported so the
// out-of-package gateway driver (traefik) resolves the SAME host dir as the
// deploy core, without deploy importing the driver.
func GatewayConfigRoot() string {
	if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
		return base
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "sklp-config")
	}
	return filepath.Join(home, ".config")
}

// GatewayDir is the HOST-level gateway config dir (NOT per-space): the singleton
// owns one shared file-provider dyn dir + cert store here, unlike
// control.spaceConfigDir which is scoped per space. Exported so the
// out-of-package driver writes its routes/state under the same dir the deploy
// core and `sklp gateway` reason about.
func GatewayDir() string {
	return filepath.Join(GatewayConfigRoot(), "sklp", "gateway")
}
