package spacemanager

import (
	"context"

	"github.com/lalternativefabrique/sklp/space"
	"github.com/lalternativefabrique/sklp/space/ociproc"
)

// runcLauncher adapts space.Space (runc-backed, real containers) to
// the deploy.Launcher interface. It is the production launcher; tests use
// execLauncher instead to avoid needing container privileges.
type runcLauncher struct {
	rt    *space.Space
	auths map[string]space.RegistryAuth // private-registry pull creds, attached to each Start ctx
}

// SetRegistryAuths merges resolved private-registry pull credentials into the
// set the launcher injects into every Start's context. Merge, never replace:
// an image roll re-applies the on-disk manifest, which by design carries no
// password, so a replace would wipe the credentials a full apply shipped and
// every later pull of a private image would fail. An empty map is a no-op.
func (l *runcLauncher) SetRegistryAuths(auths map[string]space.RegistryAuth) {
	if len(auths) == 0 {
		return
	}
	if l.auths == nil {
		l.auths = make(map[string]space.RegistryAuth, len(auths))
	}
	for host, a := range auths {
		l.auths[host] = a
	}
}

// NewRuncLauncher builds the runc-backed launcher and the matching readiness
// checker for the real agent: both share one Space so the checker
// resolves the same per-service probe coordinates the launcher created.
// spaceName names the space whose shared bridge network every service
// joins (reachable by service name); "" yields standalone services.
func NewRuncLauncher(spaceName string) (Launcher, readyChecker, error) {
	return NewRuncLauncherWithEgress(spaceName, nil)
}

// NewRuncLauncherWithEgress is NewRuncLauncher with the space's Internet
// allowlist applied before any service starts — the space network is built
// once on first Start, so a later call would be ignored.
//
// Every space a deploy launcher runs has a root of its own (ADR-022). dev,
// CI and space up build their spaces elsewhere and keep the shared model.
func NewRuncLauncherWithEgress(spaceName string, allowEgress []string) (Launcher, readyChecker, error) {
	rt, err := space.NewSpace(spaceName)
	if err != nil {
		return nil, nil, err
	}
	if err := rt.UseChildUserns(); err != nil {
		return nil, nil, err
	}
	if len(allowEgress) > 0 {
		rt.RestrictEgress(allowEgress)
	}
	return &runcLauncher{rt: rt}, runcReadyChecker{rt: rt}, nil
}

func (l *runcLauncher) Start(ctx context.Context, svc Service) (int, error) {
	// Attach the private-registry pull creds so the image resolver authenticates
	// from them (preferred over the stored credentials). No-op when unset.
	ctx = space.WithRegistryAuths(ctx, l.auths)
	return l.rt.Start(ctx, space.DeployService{
		Name:         svc.Name,
		Image:        svc.Image,
		Command:      svc.Command,
		Env:          svc.Env,
		Volumes:      svc.Volumes,
		Port:         svc.Port,
		Capabilities: svc.Capabilities,
		Resources: ociproc.Limits{
			CPUCores:    svc.Resources.CPUCores,
			MemoryBytes: svc.Resources.MemoryBytes,
			PidsMax:     svc.Resources.PidsMax,
		},
		RequireResources: svc.Resources.Require,
	})
}

func (l *runcLauncher) Stop(id string) { l.rt.Stop(id) }

// ExecCoords returns the runc bin/state-root/container-id for instance id, so
// the agent can `runc exec` into a live instance (sklp deploy exec). It satisfies
// the agent's optional execProvider; the exec test launcher implements no such
// method, so exec is simply unavailable there.
func (l *runcLauncher) ExecCoords(id string) (runcBin, runcRoot, containerID string) {
	return l.rt.ExecCoords(id)
}

// LogPath is the host-side file capturing instance id's container stdout/stderr
// (runtime startDetached writes it). It satisfies the agent's logPathProvider so
// container logs are streamed to the CLI; an empty path disables it for an id.
func (l *runcLauncher) LogPath(id string) string { return l.rt.LogPath(id) }

// Close tears the space network down after every container is stopped. It
// satisfies the optional `closer` the agent's teardown type-asserts; a no-op
// for a standalone launcher.
func (l *runcLauncher) Close() { l.rt.Close() }

// Addr returns the internal address the proxy routes to for instance id.
//
// LIMITATION (documented, deliberately not over-built for this milestone): the
// runc Space does not yet resolve a per-container IP, so we return the
// stable runc container id as the host the proxy targets. The real wiring —
// resolving each container's veth/slirp IP and joining the proxy to the same
// network namespace — is a network concern left for the production traefik
// integration and is NOT exercised by tests (no network/privilege in CI). The
// agent treats this as an opaque address; only its identity and change-detection
// matter to the reconciliation/rolling logic.
func (l *runcLauncher) Addr(id string) string {
	return l.rt.ContainerID(id)
}

// IPPort returns the service's current container IP and declared port:, or
// ok=false when the space has no bridge network or the container's IP isn't
// known yet (not yet attached). Read-only: it never mutates Space state, and
// Space itself has no knowledge of who calls this — the Network Controller
// polls it from here, at the spacemanager layer, precisely so Space stays
// independent of any controller wiring.
func (l *runcLauncher) IPPort(name string) (ip string, port int, ok bool) {
	c := l.rt.ProbeCoords(name)
	if c.ContainerIP == "" {
		return "", 0, false
	}
	return c.ContainerIP, c.Port, true
}

// OpenIngress binds the host listeners for serviceName's port mappings and
// proxies host traffic into the space (round-robin across Ready replicas,
// dialed in the space netns). It satisfies the agent's optional
// ingressController; a no-op for a standalone launcher (no space net).
func (l *runcLauncher) OpenIngress(serviceName string, mappings []space.PortMapping) {
	l.rt.OpenIngress(serviceName, mappings)
}

// GatewayBackend resolves a host-reachable backend ("127.0.0.1:<hostPort>") for
// a DOMAINED service, opening a gateway loopback ingress into the space netns on
// first call (memoised). It satisfies the agent's optional gatewayBackender so
// the agent can ask the launcher for a domained service's backend WITHOUT a
// user-declared host port — the host→netns bridge that replaces Addr()'s
// container-id stub for the gateway path. serviceName is the space DNS service
// name; containerPort is the service's exposed port. The exec test launcher
// implements no such method (no real netns), so the agent's gateway path is
// inert there. Addr() is left untouched — the intra-space proxy keeps its
// container-id-as-opaque-id meaning.
func (l *runcLauncher) GatewayBackend(serviceName string, containerPort int) (string, error) {
	return l.rt.GatewayBackend(serviceName, containerPort)
}

// SetL7Routes pushes the native L7 ingress routing table to the space worker
// (which hot-swaps its in-netns reverse proxy). It satisfies the agent's
// optional l7Router; the exec test launcher implements no such method, so the
// agent's L7 path is inert there.
func (l *runcLauncher) SetL7Routes(routesJSON string) error {
	return l.rt.SetL7Routes(routesJSON)
}

// SetACLEdges pushes the declared depends_on graph to the space's east-west
// forward-chain ACL. It satisfies the agent's optional aclApplier; the exec
// test launcher implements no such method, so the agent's ACL push is inert
// there.
func (l *runcLauncher) SetACLEdges(edges []ACLEdge) error {
	converted := make([]space.ACLEdge, len(edges))
	for i, e := range edges {
		converted[i] = space.ACLEdge{From: e.From, To: e.To}
	}
	return l.rt.SetACLEdges(converted)
}
