package spacemanager

import "github.com/lalternativefabrique/sklp/space"

// ingressController is the injected host->space ingress front-end, mirroring
// the proxyController/Launcher pattern. The production impl (the runc launcher)
// binds host loopback listeners and splices each connection into the space
// netns (round-robin across replicas); tests inject a fake that records which
// services were opened, so the agent's "open on first Ready" lifecycle is
// asserted without a real network or privilege.
//
// OpenIngress is idempotent per service: the agent calls it the first time a
// service with port mappings reaches Ready (at least one replica up). serviceName
// is the space DNS service name; mappings are the parsed host:container ports.
type ingressController interface {
	OpenIngress(serviceName string, mappings []space.PortMapping)
}

// gatewayBackender is the optional launcher capability the agent type-asserts to
// resolve a host-reachable backend for a DOMAINED service (one with a public
// hostname but no user-declared host port). The runc launcher implements it by
// opening a loopback gateway ingress into the space netns and returning
// "127.0.0.1:<hostPort>"; the exec test launcher does not (no real netns), so the
// agent's gateway-backend path is inert there. It is SEPARATE from Launcher.Addr
// (the intra-space proxy address) so G2 adds the gateway path without changing
// the existing proxy meaning. The gateway driver (G3) consumes the returned addr
// as Domain.Backends — pure transport, no vendor types cross this boundary.
type gatewayBackender interface {
	GatewayBackend(serviceName string, containerPort int) (string, error)
}

// l7Router is the optional launcher capability the agent type-asserts to push
// the native in-worker L7 ingress routing table (JSON-encoded []ingress.Route)
// to the space worker. The runc launcher implements it (delegating to the space
// worker's l7routes command); the exec test launcher does not, so the agent's L7
// push is inert there. It is the native analogue of the gateway driver's route
// file write — no host port, no container, the worker reverse-proxies in-netns.
type l7Router interface {
	SetL7Routes(routesJSON string) error
}

// aclApplier is the optional launcher capability the agent type-asserts to
// push the declared depends_on graph to the space's east-west forward-chain
// ACL. The runc launcher implements it (delegating to the space worker's acl
// command); the exec test launcher does not (no real netns), so the agent's
// ACL push is inert there — an untested-launcher Space simply keeps the flat
// east-west it always had. edges are (caller, target) service-name pairs;
// the launcher resolves names to live IPs at push time, so the ruleset always
// targets the CURRENT Ready replica set, not a stale snapshot.
type aclApplier interface {
	SetACLEdges(edges []ACLEdge) error
}

// ACLEdge is one declared depends_on edge: caller service name -> target
// service name. Exported so config.go (which builds the graph from
// deploy.yaml) and the launcher (which resolves it to IPs) share one type
// without either importing the other's internals.
type ACLEdge struct {
	From string
	To   string
}

// toRuntimePortMappings converts the deploy-layer parsed mappings into the
// runtime shape the launcher's host ingress consumes.
func toRuntimePortMappings(in []PortMapping) []space.PortMapping {
	if len(in) == 0 {
		return nil
	}
	out := make([]space.PortMapping, 0, len(in))
	for _, m := range in {
		out = append(out, space.PortMapping{BindHost: m.BindHost, HostPort: m.Host, ContainerPort: m.Container, ProxyProtocol: m.ProxyProtocol, UDP: m.UDP})
	}
	return out
}
