package netctrl

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lalternativefabrique/sklp/ingress"
	"github.com/lalternativefabrique/sklp/spacemanager"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
	"github.com/lalternativefabrique/sklp/spacemanager/mesh"
)

// controllerIngressBase is the first host port the Reconciler allocates for
// its own 127.0.0.1 ingress into a service — separate from tcpRouter's own
// allocator range (tcpAllocatorBase) so the two never collide.
const controllerIngressBase = 21000

// Reconciler polls, on an interval, EVERY persisted Space's manifest (what
// SHOULD be registered, per its declared port:) against its live Supervisor
// snapshot (what IS actually Ready, and at which IP), and syncs the
// Controller's registry to match. This is the pull side of the design: the
// Controller discovers state on its own schedule, entirely independent of
// space.Space — no Space ever calls into this package.
//
// A service's container IP (e.g. 10.88.0.26) lives inside that Space's own
// isolated netns and is NOT dialable from the host netns the Controller runs
// in — only the Space's own worker can reach it. So instead of registering
// the container IP directly, the Reconciler opens a host-side ingress
// listener (127.0.0.1:<allocated port> -> the container) via the
// Supervisor's OpenControllerIngress, and registers THAT address — the same
// host->space splice mechanism `ingress:` already uses, just triggered
// automatically from port: alone instead of a manifest declaration.
type Reconciler struct {
	ctrl *Controller
	reg  *control.Registry

	// known tracks what this Reconciler last Put, so a service that
	// disappears from a later poll (removed from the manifest, or gone
	// Unready with no replacement) gets Delete'd exactly once.
	known map[string]bool // key: "<space>.<service>"

	// ingressPort memoises the host port allocated per service, so a repeat
	// tick reuses the same 127.0.0.1:<port> — OpenControllerIngress is
	// idempotent per (space,service) but the port itself must stay stable
	// across polls or the Registry entry would need updating every tick.
	ingressPort map[string]int // key: "<space>.<service>"
	nextPort    int

	// Mesh fields, all nil/empty when mesh is disabled (WithMesh never
	// called) — the Reconciler then behaves exactly as it did before
	// cross-node support existed: local-only, no re-registration, no peer
	// cache refresh.
	mesh       *mesh.Client
	nodeID     string
	pubkey     string
	endpoint   string
	selfSpaces []string

	// syncPeers, when set (WithPeerSync), pushes a freshly fetched peer
	// list into this node's wg0 config — without it the WireGuard peer set
	// is frozen at boot-time Join, and a node joining the mesh later is
	// resolved by DNS but unreachable (its pubkey unknown to wg0) until
	// every older node restarts. Injected as a closure so netctrl never
	// imports the wireguard plumbing itself. lastPeerSig skips the (rare)
	// device write when nothing tunnel-relevant changed.
	syncPeers   func([]mesh.Peer) error
	onPeers     func() // notified after each peer-cache refresh (mesh route re-resolution)
	lastPeerSig string
}

// NewReconciler builds a Reconciler. reg is the same control.Registry RunAgent
// already builds and registers every space's Supervisor into — the Reconciler
// never opens a new connection or listener of its own (beyond the per-service
// ingress ports it allocates), it only reads what's already live in-process.
func NewReconciler(ctrl *Controller, reg *control.Registry) *Reconciler {
	return &Reconciler{
		ctrl:        ctrl,
		reg:         reg,
		known:       map[string]bool{},
		ingressPort: map[string]int{},
		nextPort:    controllerIngressBase,
	}
}

// WithMesh enables cross-node registration: on every tick, after syncing
// its local Registry, the Reconciler ALSO re-registers this node's current
// service list with meshauthority (via client) and refreshes the
// Controller's peer cache from the returned peer list. nodeID/pubkey/
// endpoint/selfSpaces mirror exactly what mesh.Join already registered for
// this node's topology at boot — Register is a whole-value upsert by ID, so
// re-sending the same identity fields alongside a fresh service list is
// the correct, idempotent way to keep both in sync. The mesh call is
// best-effort — a failure is logged and the tick continues, never
// aborted, same posture as reconcileSpace's own per-space errors.
func (r *Reconciler) WithMesh(client *mesh.Client, nodeID, pubkey, endpoint string, selfSpaces []string) *Reconciler {
	r.mesh = client
	r.nodeID = nodeID
	r.pubkey = pubkey
	r.endpoint = endpoint
	r.selfSpaces = selfSpaces
	return r
}

// WithPeerSync enables periodic wg0 peer-set sync (see the syncPeers field
// doc): fn receives the full peer list fetched on each successful mesh
// registration, self included (the sync layer excludes it, same as Join's
// own boot-time call). Only set this when wg0 actually exists — a node
// whose boot-time Join failed has no interface to configure.
// WithRouteRefresher registers a callback invoked after every peer-cache
// refresh, so mesh-targeted gateway routes re-resolve against the newest
// peer set. The callee change-detects; calling it every tick is cheap.
func (r *Reconciler) WithRouteRefresher(fn func()) *Reconciler {
	r.onPeers = fn
	return r
}

func (r *Reconciler) WithPeerSync(fn func([]mesh.Peer) error) *Reconciler {
	r.syncPeers = fn
	return r
}

// Run polls every interval until ctx is cancelled. Errors reading a manifest
// or space state are logged and skipped — one broken space must never stop
// reconciliation for the others.
func (r *Reconciler) Run(stop <-chan struct{}, interval time.Duration) {
	r.tick()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			r.tick()
		}
	}
}

// tick runs one reconciliation pass over every persisted Space.
func (r *Reconciler) tick() {
	spaces, err := control.ListPersistedSpaces()
	if err != nil {
		fmt.Fprintf(os.Stderr, "netctrl: reconcile: list spaces: %v\n", err)
		return
	}
	seen := map[string]bool{}
	var liveServices []mesh.ServiceRef
	for _, space := range spaces {
		liveServices = append(liveServices, r.reconcileSpace(space, seen)...)
	}
	// Anything previously Put but not seen this pass (space torn down,
	// manifest no longer declares it, or the space's Supervisor vanished
	// from the Registry) is stale — remove it.
	for k := range r.known {
		if !seen[k] {
			space, service := splitKey(k)
			r.ctrl.Delete(space, service)
			delete(r.known, k)
		}
	}

	if r.mesh != nil {
		r.syncMesh(liveServices)
	}
}

// syncMesh re-registers this node's current service list with meshauthority
// and refreshes the Controller's peer cache from the response. Best-effort:
// a failure here never aborts the tick — the local Registry is already
// consistent regardless of mesh reachability.
func (r *Reconciler) syncMesh(services []mesh.ServiceRef) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	self, peers, err := r.mesh.Register(ctx, r.nodeID, r.pubkey, r.endpoint, r.selfSpaces, services)
	if err != nil {
		fmt.Fprintf(os.Stderr, "netctrl: reconcile: mesh register: %v\n", err)
		return
	}
	if err := mesh.RememberPeers(self.PrivateIP, peers); err != nil {
		fmt.Fprintf(os.Stderr, "netctrl: reconcile: cache peer list: %v\n", err)
	}
	r.ctrl.Peers().Refresh(peers)
	if r.onPeers != nil {
		r.onPeers()
	}

	if r.syncPeers != nil {
		if sig := peerSignature(peers); sig != r.lastPeerSig {
			if err := r.syncPeers(peers); err != nil {
				fmt.Fprintf(os.Stderr, "netctrl: reconcile: wg peer sync: %v\n", err)
			} else {
				r.lastPeerSig = sig
			}
		}
	}
}

// peerSignature folds the tunnel-relevant peer fields (identity, pubkey,
// endpoint, mesh IP — NOT services or last-seen, which change every tick
// without affecting wg0) into a comparable string, so syncMesh only
// rewrites the WireGuard device when the peer topology actually changed.
func peerSignature(peers []mesh.Peer) string {
	parts := make([]string, 0, len(peers))
	for _, p := range peers {
		parts = append(parts, p.ID+"|"+p.PubKey+"|"+p.Endpoint+"|"+p.PrivateIP)
	}
	sort.Strings(parts)
	return strings.Join(parts, "\n")
}

// reconcileSpace compares one space's declared ports against its live
// Supervisor snapshot, Put'ing every service that is both declared AND
// Ready, marking each as seen (so tick's sweep doesn't Delete it), and
// returning the (space, service, containerPort) triples just reconciled —
// the manifest-declared port, NOT the local 127.0.0.1:hostPort ingress
// port, since that is meaningless to a remote peer.
func (r *Reconciler) reconcileSpace(space string, seen map[string]bool) []mesh.ServiceRef {
	services, meta, err := control.LoadManifest(space)
	if err != nil {
		fmt.Fprintf(os.Stderr, "netctrl: reconcile: load manifest %q: %v\n", space, err)
		return nil
	}
	declaredPort := map[string]int{}
	for _, svc := range services {
		if svc.Port > 0 {
			declaredPort[svc.Name] = svc.Port
		}
	}
	if len(declaredPort) == 0 {
		return nil
	}

	sup, ok := r.reg.Supervisor(space)
	if !ok {
		return nil // space not yet materialised on this node
	}

	var live []mesh.ServiceRef
	for _, ss := range sup.Snapshot() {
		containerPort, declared := declaredPort[ss.Name]
		if !declared {
			continue
		}
		ready := readyInstance(ss)
		if ready == nil {
			continue
		}
		if ready.IP == "" {
			continue // Ready but IP not yet resolved: wait for the next tick
		}

		k := key(space, ss.Name)
		hostPort := r.controllerPort(k)
		sup.OpenControllerIngress(ss.Name, hostPort, containerPort)

		if perr := r.ctrl.Put(space, ss.Name, "127.0.0.1", hostPort, containerPort); perr != nil {
			// A failed TCP bind must not drop the service: HTTP and in-space
			// access stay registered, only the raw-TCP path is missing. The
			// router remembers the refusal, so this reports the clash once
			// rather than on every tick.
			fmt.Fprintf(os.Stderr, "netctrl: reconcile: %s keeps HTTP but has no TCP listener: %v\n", k, perr)
		}
		r.known[k] = true
		seen[k] = true
		live = append(live, mesh.ServiceRef{Space: space, Service: ss.Name, Port: containerPort})
		r.registerPassthroughs(space, ss.Name, passthroughPorts(ss.Name, containerPort, meta.Routes), sup, seen)
	}
	return append(live, r.registerSpaceGateway(space, sup, seen))
}

// registerPassthroughs opens a raw TCP listener on each TLS passthrough port
// of service, so a front splicing by SNI reaches the container undecrypted.
func (r *Reconciler) registerPassthroughs(space, service string, ports []int, sup control.Supervisor, seen map[string]bool) {
	for _, port := range ports {
		name := service + ":" + strconv.Itoa(port)
		k := key(space, name)
		hostPort := r.controllerPort(k)
		sup.OpenControllerIngress(service, hostPort, port)
		if err := r.ctrl.PutTCP(space, name, "127.0.0.1", hostPort, port); err != nil {
			fmt.Fprintf(os.Stderr, "netctrl: reconcile: %s has no passthrough listener: %v\n", k, err)
		}
		r.known[k] = true
		seen[k] = true
	}
}

// passthroughPorts lists the passthrough ports of service needing a listener of
// their own: the declared port already has one, and binding it twice fails.
func passthroughPorts(service string, declared int, routes []spacemanager.ResolvedRoute) []int {
	var ports []int
	seen := map[int]bool{declared: true}
	for _, rt := range routes {
		if rt.Passthrough == 0 || rt.Service != service || seen[rt.Passthrough] {
			continue
		}
		seen[rt.Passthrough] = true
		ports = append(ports, rt.Passthrough)
	}
	return ports
}

// registerSpaceGateway exposes the space's native L7 ingress through the
// controller like a service, under its reserved name. A front that hands a
// whole public host to this space (ADR-021) reaches it here, and the space's
// own routes decide the backend.
func (r *Reconciler) registerSpaceGateway(space string, sup control.Supervisor, seen map[string]bool) mesh.ServiceRef {
	k := key(space, ingress.GatewayService)
	hostPort := r.controllerPort(k)
	sup.OpenControllerIngress(ingress.GatewayService, hostPort, ingress.GatewayPort)
	r.ctrl.PutHTTP(space, ingress.GatewayService, "127.0.0.1", hostPort)
	r.known[k] = true
	seen[k] = true
	return mesh.ServiceRef{Space: space, Service: ingress.GatewayService, Port: ingress.GatewayPort}
}

// controllerPort returns the stable loopback port the controller ingress for
// k listens on, allocating one on first use.
func (r *Reconciler) controllerPort(k string) int {
	hostPort, ok := r.ingressPort[k]
	if !ok {
		hostPort = r.nextPort
		r.nextPort++
		r.ingressPort[k] = hostPort
	}
	return hostPort
}

// readyInstance returns the first Ready instance with a resolved IP, or nil
// if the service has none yet — a service mid-rollout or with all replicas
// unready is simply not registered until one becomes reachable.
func readyInstance(ss spacemanager.ServiceSnapshot) *spacemanager.InstanceSnapshot {
	for i := range ss.Instances {
		if ss.Instances[i].State == spacemanager.Ready {
			return &ss.Instances[i]
		}
	}
	return nil
}

// splitKey reverses key(space, service) — Registry.key/netctrl.key both
// lowercase and join with ".", so this only needs to find the first
// separator (space/service names never contain ".").
func splitKey(k string) (space, service string) {
	space, service, _ = strings.Cut(k, ".")
	return space, service
}
