package sklet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lalternativefabrique/sklp/internal/config"
	"github.com/lalternativefabrique/sklp/sklet/logship"
	"github.com/lalternativefabrique/sklp/sklet/publicnic"
	"github.com/lalternativefabrique/sklp/space"
	"github.com/lalternativefabrique/sklp/space/spacenet"
	"github.com/lalternativefabrique/sklp/spacemanager"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
	"github.com/lalternativefabrique/sklp/spacemanager/controller"
	"github.com/lalternativefabrique/sklp/spacemanager/mesh"
	"github.com/lalternativefabrique/sklp/spacemanager/netctrl"
	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

// agent.go implements `sklet agent` — the control-plane node mode.
//
// A normal apply needs a manifest to exist before the control API comes up: the
// API is a listener bolted onto a space's supervisor, so there is no way to
// stand up a node that merely WAITS to be told what to run. That inverts the
// kube model, where a kubelet boots empty and the control plane pushes
// workloads to it.
//
// `sklet agent` fixes that: it starts a supervisor with ZERO services,
// generates its self-signed cert + Bearer token, and opens the control API on
// --listen. It then blocks, waiting for a remote `sklp deploy <stack> --server`
// to POST /v1/apply, which the apply handler treats as the FIRST deploy
// (wasEmpty → created) and materialises the space in place. No repo, no
// manifest, and nothing done locally are required on the node — the node only
// generates the credentials needed to be reached.
//
// The node's cert and token are host-wide (see tls.go/token.go): an empty node
// runs no space at all until its first apply materialises one.

// RunAgent stands up the control-plane node: one supervisor per space this
// host already runs (discovered from persisted manifests — control.
// ListPersistedSpaces); an empty node runs none and only serves the control
// API, and any cache dir a previous first boot left for a space that holds
// nothing is removed. It opens ONE network control
// API on listen, shared by every space (dispatched by SpaceRoutePrefix —
// see control.ServeNetMulti), and blocks until SIGINT/SIGTERM. The control
// API's /v1/apply against an ALREADY-adopted space's own path reconciles it
// in place, same as before. A remote deploy naming a space this boot never
// adopted (a NEW space added to a multi-space node) is materialised on
// demand by the network listener's Materializer (see the materialize
// closure below and control.newMultiMux) — no sklet-agent restart required.
// MeshOptions configures this node's join into the cross-VM WireGuard mesh
// (ADR-006). AuthorityURL empty disables the mesh entirely — the node runs
// exactly as before.
type MeshOptions struct {
	AuthorityURL string
	Token        string
	Endpoint     string
	// CertFingerprint pins meshauthority's self-signed certificate (SHA-256
	// hex); required, see mesh.CheckAuthority.
	CertFingerprint string
	// RelayDomain, on a front, makes the control API listener splice any
	// connection whose SNI is "<node-id>.<RelayDomain>" to that peer's own
	// control API over the private network (ADR-019).
	RelayDomain string
	// AdvertiseVia, on a private origin, advertises "<own node-id>.<AdvertiseVia>"
	// as this node's server, the name the front relays.
	AdvertiseVia string
}

func RunAgent(listen, advertise, gatewayMode string, meshOpts MeshOptions, publicNIC *publicnic.Config) error {
	persisted, lerr := control.ListPersistedSpaces()
	if lerr != nil {
		return fmt.Errorf("list persisted spaces: %w", lerr)
	}
	if removed, rerr := space.RemoveEmptyShells(persisted); rerr != nil {
		fmt.Fprintf(os.Stderr, "sklet agent: empty space dirs left in place: %v\n", rerr)
	} else if len(removed) > 0 {
		fmt.Fprintf(os.Stdout, "sklet agent: removed empty space dir(s): %s\n", strings.Join(removed, ", "))
	}

	// selfMeshIP/meshClient are non-empty/non-nil whenever a mesh identity
	// exists AND the join's wg0/mesh-IP assignment succeeded — the identity
	// half (nodeID/pubkey) does NOT require Join's HTTP register call to
	// have succeeded: EnsureIdentity is a pure local operation (a persisted
	// keypair), so meshClient can still be built and handed to the
	// Reconciler even when this boot's Join attempt hit a transient
	// meshauthority failure (e.g. its own backing store not up yet). The
	// Reconciler's own periodic Register (see WithMesh) then retries on its
	// normal polling cadence instead of only at the next process restart.
	var selfMeshIP, meshNodeID, meshPubkey string
	var meshClient *mesh.Client
	var meshIdentity mesh.Identity
	if meshOpts.AuthorityURL != "" {
		if err := mesh.CheckAuthority(meshOpts.AuthorityURL, meshOpts.CertFingerprint); err != nil {
			return fmt.Errorf("%w: %v", spacemanager.ErrConfig, err)
		}
		// Identity first: the client signs every write with this node's key.
		if id, ierr := mesh.EnsureIdentity(); ierr != nil {
			fmt.Fprintf(os.Stderr, "sklet agent: mesh identity (continuing without mesh support): %v\n", ierr)
		} else {
			meshClient = mesh.NewPinnedClient(meshOpts.AuthorityURL, meshOpts.Token, id.SigningKey, meshOpts.CertFingerprint)
			control.InstallMeshDirectory(meshDirectory{meshClient})
			meshIdentity = id
			meshNodeID = id.NodeID
			meshPubkey = id.PrivateKey.PublicKey().String()
			ip, err := mesh.Join(context.Background(), meshOpts.AuthorityURL, meshOpts.Token, meshOpts.CertFingerprint, meshOpts.Endpoint, persisted, nil)
			if err != nil && ip == "" {
				// Non-fatal: a node that fails to join the mesh still serves
				// its spaces over the existing public-front-door path
				// (ADR-005 §9). No wg0 and no MeshIP for the Network
				// Controller's dual-bind this run; the Reconciler keeps
				// retrying Register on its own cadence below, and wg0 comes
				// up on the next process restart.
				fmt.Fprintf(os.Stderr, "sklet agent: mesh join failed (continuing without it): %v\n", err)
			} else if err != nil {
				// Degraded: meshauthority was unreachable but wg0 came up from
				// the last known peer list, so the mesh still carries traffic
				// through this boot. A peer whose key changed meanwhile stays
				// unreachable until the Reconciler lands a Register.
				fmt.Fprintf(os.Stderr, "sklet agent: %v\n", err)
				selfMeshIP = ip
			} else {
				selfMeshIP = ip
			}
		}
	}

	if meshOpts.AdvertiseVia != "" && advertise == "" {
		nodeID := meshNodeID
		if nodeID == "" {
			if id, ierr := mesh.EnsureIdentity(); ierr != nil {
				return fmt.Errorf("mesh identity for --advertise-via: %w", ierr)
			} else {
				nodeID = id.NodeID
			}
		}
		advertise = nodeID + "." + strings.Trim(meshOpts.AdvertiseVia, ".")
	}

	// Write a standalone, importable node config next to the control creds
	// (~/.config/sklp/node-config.yaml): a single `default` deploy context with the
	// token, cert fingerprint and space, so the operator scps ONE file per VM
	// and points sklp at it (XDG_CONFIG_HOME=<dir> or copy over config.yaml) —
	// no `sklp deploy login`, no hand-copying token/fingerprint. server is filled
	// from --advertise when given, else left blank for the operator to set. This
	// ensures the same cert/token the listener uses, so the file always matches.
	// The written context's Space names the FIRST persisted space only (none
	// on an empty node) — an operator piloting a specific space passes --space
	// explicitly; this file just gets a multi-space node's client bootstrapped.
	if cfgPath, werr := writeNodeConfig(firstOf(persisted), advertise, listen); werr != nil {
		fmt.Fprintf(os.Stderr, "sklet agent: could not write node config (control API still serves): %v\n", werr)
	} else {
		fmt.Fprintf(os.Stdout, "sklet agent: node config written to %s (scp it to your workstation)\n", cfgPath)
	}

	// Same rule as SuperviseLoop: a node agent must not die on SIGHUP. A hijacked
	// exec session hanging up, or the launching terminal closing, would
	// otherwise take every space down with the agent.
	signal.Ignore(unix.SIGHUP)
	ctx, stop := signal.NotifyContext(context.Background(), unix.SIGINT, unix.SIGTERM)
	defer stop()

	fmt.Fprintf(os.Stdout, "sklet agent: node ready with %d space(s) (%s), waiting for a remote deploy on %s\n",
		len(persisted), strings.Join(persisted, ", "), listen)

	// SuperviseLoop (the single-space path) installs this itself; RunAgent's
	// multi-space path has no equivalent per-space hook to piggyback on, so it
	// is installed once here instead — GET /v1/spaces otherwise 501s forever on
	// a node started through `sklet agent`.
	installInventory(os.Stderr)

	reg := control.NewRegistry()
	sc := controller.New()

	// The Network Controller is unconditional, unlike the mesh: every node,
	// mesh-joined or not, needs its own local DNS/HTTP/TCP mediator for
	// cross-Space traffic. It is entirely independent of space.Space — the
	// Reconciler POLLS reg (this same Registry every space's Supervisor
	// registers into below) and each space's persisted manifest, rather
	// than Space ever calling into the controller.
	var meshResolver func(space, service string) (string, bool)
	var nodeHost control.NodeHostResolver
	ctrlIP, cierr := spacenet.ControllerHostIP()
	netctrlOpts := netctrl.Options{SelfPeerID: meshNodeID}
	if selfMeshIP != "" {
		netctrlOpts.MeshIP = net.ParseIP(selfMeshIP)
	}
	if cierr != nil {
		fmt.Fprintf(os.Stderr, "sklet agent: network controller disabled, no host IP available (continuing without cross-space networking): %v\n", cierr)
	} else if ctrl, cerr := netctrl.Start(net.ParseIP(ctrlIP), netctrlOpts); cerr != nil {
		// Binding :53 needs cap_net_bind_service, which cloud-init sets on the
		// binary and which `install` drops when a new sklet is copied over the
		// old one. Degrading quietly there cost a day of every
		// <service>.<space>.internal URL silently not resolving, so the one
		// cause an operator can fix says so, at the top, in the imperative.
		if errors.Is(cerr, os.ErrPermission) {
			fmt.Fprintf(os.Stderr, "sklet agent: cross-space DNS is DOWN — %v\n", cerr)
			fmt.Fprintf(os.Stderr, "sklet agent: the binary lost its capabilities (a manual swap drops them). Restore with:\n")
			fmt.Fprintf(os.Stderr, "sklet agent:   sudo setcap '%s' %s\n", skletCaps, selfPath())
			fmt.Fprintf(os.Stderr, "sklet agent: then restart this agent. Until then every <service>.<space>.internal name fails to resolve.\n")
		} else {
			fmt.Fprintf(os.Stderr, "sklet agent: network controller disabled (continuing without cross-space networking): %v\n", cerr)
		}
	} else {
		defer ctrl.Close()
		os.Setenv(spacenet.EnvControllerIP, ctrlIP) // read by deploynet_linux.go's egress wiring and dns.go's hostUpstreams
		ctrl.OnServedPorts(53, 80, spacenet.SetControllerPorts)
		if ip := net.ParseIP(advertise); ip != nil && ip.To4() != nil {
			os.Setenv(spacenet.EnvNodePublicAddrs, ip.To4().String())
		}
		fmt.Fprintf(os.Stdout, "sklet agent: network controller listening on %s\n", ctrlIP)
		meshResolver = ctrl.Peers().RemoteEndpoint
		nodeHost = ctrl.Peers().NodeHost
		rec := netctrl.NewReconciler(ctrl, reg)
		if meshClient != nil {
			rec = rec.WithMesh(meshClient, meshNodeID, meshPubkey, meshOpts.Endpoint, persisted)
			// Mesh gateway routes re-resolve after every peer refresh: a
			// target moving nodes (or appearing) re-renders its route file.
			rec = rec.WithRouteRefresher(func() {
				for _, n := range reg.Names() {
					if sup, ok := reg.Supervisor(n); ok {
						if m, ok := sup.(interface{ RefreshMeshRoutes() }); ok {
							m.RefreshMeshRoutes()
						}
					}
				}
			})
			if selfMeshIP != "" {
				// wg0 exists (Join succeeded): keep its peer set current as
				// nodes join/leave the mesh, instead of frozen at boot.
				rec = rec.WithPeerSync(func(peers []mesh.Peer) error {
					return mesh.SyncPeers(meshIdentity.PrivateKey, meshIdentity.NodeID, peers)
				})
			}
		}
		go rec.Run(ctx.Done(), 5*time.Second)
	}

	startPublicNIC(ctx, publicNIC)
	shippers := newShipperSet()
	startRestoreController(ctx, reg, sc, gatewayMode, shippers, os.Stdout)
	go collectImagesEvery(ctx, imageGCInterval)
	opts := make([]controller.SpaceOptions, 0, len(persisted))
	for _, name := range persisted {
		o, oerr := agentSpaceOptions(name, gatewayMode, reg, shippers)
		o.MeshResolver = meshResolver
		if oerr != nil {
			return oerr
		}
		opts = append(opts, o)
	}

	// ONE network listener for every space, dispatched by SpaceRoutePrefix —
	// never one per space (they would all fight for the same port). Started
	// before RunAll blocks so it is up even while the first space's gateway is
	// still coming up; a request for a not-yet-registered space 404s until its
	// goroutine reaches NetListener, same latency window the old single-space
	// path always had between process start and control.Serve/ServeNet.
	// materialize lets an /v1/apply against a space this boot never discovered
	// (a brand-new deploy, not yet persisted when RunAgent started) bring that
	// space up on the same running sc/reg, instead of 404ing until an operator
	// restarts sklet-agent — see newMultiMux's use of it and AddSpace's doc.
	materialize := func(space string) error {
		o, oerr := agentSpaceOptions(space, gatewayMode, reg, shippers)
		o.MeshResolver = meshResolver
		if oerr != nil {
			return oerr
		}
		return sc.AddSpace(ctx, o)
	}

	if listen != "" {
		if narrowed, ok := privateOriginListen(listen, meshOpts.AdvertiseVia, meshOpts.Endpoint); ok {
			fmt.Fprintf(os.Stdout, "sklet agent: private origin, control API bound to %s instead of %s\n", narrowed, listen)
			listen = narrowed
		}
		if err := startMultiNetListener(ctx, reg, "", listen, materialize, meshOpts.RelayDomain, nodeHost); err != nil {
			fmt.Fprintf(os.Stderr, "sklet agent: network control API disabled: %v\n", err)
		}
	}

	if err := sc.RunAll(ctx, opts); err != nil {
		return err
	}
	// RunAll returns when every space goroutine has exited — the right end for
	// the single-space supervise path, but a NODE outlives its spaces: with
	// every space stopped (a deploy delete, a down, a space that simply
	// finished) the control API must stay up for the next deploy instead of
	// the process exiting 0. Three separate "agent died for no visible
	// reason" hunts on the preflight VMs all turned out to be this return.
	// Only the signal context ends the agent.
	<-ctx.Done()
	return nil
}

func firstOf(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

// agentSpaceOptions builds the controller.SpaceOptions for one of this node's
// spaces: adopts its persisted manifest (or starts empty — the first-boot
// case), resolves its gateway mode/driver exactly like the old single-space
// RunAgent did, and wires NetListener to register the space's Supervisor in
// reg instead of opening its own listener (RunAgent opens ONE, shared).
func agentSpaceOptions(space, gatewayMode string, reg *control.Registry, shippers *shipperSet) (controller.SpaceOptions, error) {
	services, meta, err := control.LoadManifest(space)
	if err != nil {
		services = nil
		meta = spacemanager.SpaceMeta{Name: space}
	}
	if meta.Name == "" {
		meta.Name = space
	}
	meta.GatewayMode = resolveGatewayMode(meta.GatewayMode, gatewayMode)

	// In per-space mode the front door is the host mux, not a driver: ensure its
	// unit up front for the same reason the singleton is armed here — the apply
	// path only writes routes to a front door that already stands. The deploy
	// path ensures it too, but only once a manifest carries routes, which an
	// empty node has not yet.
	if meta.GatewayMode == spacemanager.GatewayModePerSpace {
		if selfExe, eerr := os.Executable(); eerr != nil {
			fmt.Fprintf(os.Stderr, "sklet agent: warning: could not resolve binary for the mux: %v\n", eerr)
		} else if merr := control.EnsureMuxUnit(selfExe, control.MuxListenAddr()); merr != nil {
			fmt.Fprintf(os.Stderr, "sklet agent: warning: could not ensure the gateway mux: %v\n", merr)
		}
	}

	return controller.SpaceOptions{
		Meta:     meta,
		Services: services,
		Out:      os.Stdout,
		ErrOut:   os.Stderr,
		// A remote `sklp deploy delete` (POST /v1/down, see RouteDown) reaches
		// this space only through Shutdown/cancel — there is no equivalent of
		// the LOCAL sklet.Down path's explicit RemoveSpaceArtifacts call here.
		// Without this, the manifest survived the stop, so a later `apply` with
		// an unchanged spec reported "unchanged" against a space whose runtime
		// was actually gone, and a fresh redeploy required a full agent
		// restart to pick the manifest back up.
		PurgeOnShutdown: true,
		// Arm the gateway UP FRONT — the node is a control plane that must be ready
		// to route the moment a remote deploy adds a domained service. It can't be
		// opened lazily: the apply path only writes routes to an ALREADY-wired
		// gateway (Agent.SetRoutes no-ops when a.gateway is nil), it does not
		// acquire one. So an empty node that waited for routes would accept the
		// deploy, start the service, yet never route it (traefik up, dynamic file
		// empty -> 404). Use the manifest's driver when it declares routes, else the
		// default, so a fresh node always has its front door standing.
		GatewayDriver: agentGatewayDriver(meta),
		// Listen deliberately left "": RunAgent opens ONE shared network listener
		// (startMultiNetListener), not one per space. NetListener now fires
		// regardless of Listen (see controller.go's runSpace) — it is purely a
		// "this space's Supervisor is ready" hook here, used to register into
		// the shared reg instead of opening a per-space listener.
		// logship.Start also happens here, not once for the node: this mode
		// replaced the per-space StartNetListener (which owned that call), and a
		// Shipper tails exactly one space's Supervisor.
		NetListenerWithDelete: func(lctx context.Context, space string, sup control.Supervisor, shutdown, purgeShutdown, deleteShutdown func()) {
			reg.RegisterWithDelete(space, sup, shutdown, purgeShutdown, deleteShutdown)
			shippers.start(lctx, space, control.NodeName(), sup)
		},
		// Without this, a torn-down space's Supervisor stays reachable in reg:
		// a subsequent POST /v1/apply for the same name dispatches to the
		// STALE Supervisor (whose DesiredServiceNames() still lists the old
		// service) instead of materializing fresh via `materialize`, so an
		// identical manifest compares equal and reports "unchanged" against a
		// space that, on disk, no longer exists.
		OnStopped: func(space string) {
			reg.Unregister(space)
			shippers.stop(space)
		},
	}, nil
}

// startMultiNetListener brings up ServeNetMulti (one TCP+TLS control API
// dispatching to every space in reg) exactly once for the whole node, mirror
// of the old per-space StartNetListener but registry-backed instead of
// single-Supervisor. certSpace names which space's cert/token to load —
// EnsureServerCert/EnsureToken ignore the argument (the identity is already
// GLOBAL, shared host-wide: see tls.go/token.go), so any persisted space
// name resolves the same file; the caller passes the node's first persisted
// space (or its --space default) as a stable, always-available choice.
// privateOriginListen narrows a wildcard --listen on a private origin to the
// private-network address the front relays to (ADR-019). Such a node is only
// ever reached through that relay, so every other interface it might bind —
// a public one included — is surface with no legitimate client. A --listen
// that already names an address is the operator's choice and is kept.
func privateOriginListen(listen, advertiseVia, meshEndpoint string) (string, bool) {
	if advertiseVia == "" || meshEndpoint == "" {
		return "", false
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil || (host != "" && host != "0.0.0.0" && host != "::") {
		return "", false
	}
	privateIP, _, err := net.SplitHostPort(meshEndpoint)
	if err != nil || net.ParseIP(privateIP) == nil {
		return "", false
	}
	return net.JoinHostPort(privateIP, port), true
}

func startMultiNetListener(ctx context.Context, reg *control.Registry, certSpace, addr string, materialize control.Materializer, relayDomain string, nodeHost control.NodeHostResolver) error {
	cert, fingerprint, err := control.EnsureServerCert(certSpace)
	if err != nil {
		return fmt.Errorf("cert: %w", err)
	}
	token, err := control.EnsureToken(certSpace)
	if err != nil {
		return fmt.Errorf("token: %w", err)
	}
	fmt.Fprintf(os.Stdout, "sklet: control API listening on https://%s\n", addr)
	tokenPath := control.TokenPath(certSpace)
	fmt.Fprintf(os.Stdout, "  token file:  %s\n", tokenPath)
	fmt.Fprintf(os.Stdout, "  fingerprint: %s\n", fingerprint)
	tcp, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	var ln net.Listener = tcp
	if relayDomain != "" && nodeHost == nil {
		fmt.Fprintf(os.Stderr, "sklet: control relay disabled, the network controller is not running; the front's own control API stays up\n")
	}
	if relayDomain != "" && nodeHost != nil {
		_, port, _ := net.SplitHostPort(addr)
		relay := control.NewRelayListener(tcp, relayDomain, port, nodeHost)
		relay.OnError(func(rerr error) { fmt.Fprintf(os.Stderr, "sklet: control relay: %v\n", rerr) })
		ln = relay
		fmt.Fprintf(os.Stdout, "  relaying:    *.%s -> peer control API\n", relayDomain)
	}
	go func() {
		if serr := control.ServeNetMultiOn(ctx, reg, ln, cert, token, materialize); serr != nil {
			fmt.Fprintf(os.Stderr, "sklet: network control API stopped: %v\n", serr)
		}
	}()
	return nil
}

// resolveGatewayMode decides the front-door mode for this boot: the mode a
// prior deploy already recorded on the manifest wins over the (now mandatory)
// --gateway-mode flag. Applying the flag unconditionally on every restart would
// let an operator's unit-file edit (or an intentional flag change with a stale
// unit reload order) silently overwrite whatever mode an actual deploy had
// recorded, flipping the front door on every `systemctl restart` and leaving
// e.g. a singleton deploy's routes unreachable (traefik's dynamic dir stays
// empty, every request 404s) even though nothing about the deploy itself
// changed. Only an empty node (no manifest, persistedMode == "") takes the flag.
func resolveGatewayMode(persistedMode, flagMode string) string {
	if persistedMode != "" {
		return persistedMode
	}
	return flagMode
}

// agentGatewayDriver picks the front-door driver for a control-plane node: the
// manifest's resolved driver when it already declares gateway routes (an adopted
// deploy), otherwise the default — so an EMPTY node still stands up its gateway
// ready for the first routed deploy, rather than coming up with none.
func agentGatewayDriver(meta spacemanager.SpaceMeta) string {
	// Per-space mode has no driver to open: the front door is the host mux and
	// TLS is terminated by the in-worker L7. Returning a driver here would stand
	// up a traefik the node never routes through.
	if meta.GatewayMode == spacemanager.GatewayModePerSpace {
		return ""
	}
	if d := GatewayDriverFor(meta, spacemanager.DefaultGatewayDriver); d != "" {
		return d
	}
	return spacemanager.DefaultGatewayDriver
}

// writeNodeConfig materialises a standalone, importable config file for this
// node next to the persisted control creds. It ensures the SAME cert + token
// the network listener will use (EnsureServerCert/EnsureToken are idempotent),
// then writes a one-context config (`default`). server is set from advertise
// (host only — the port is taken from the listen addr by the operator, or they
// paste the full URL) when provided, else blank. Returns the written path.
func writeNodeConfig(space, advertise, listen string) (string, error) {
	_, fingerprint, err := control.EnsureServerCert(space)
	if err != nil {
		return "", fmt.Errorf("ensure cert: %w", err)
	}
	token, err := control.EnsureToken(space)
	if err != nil {
		return "", fmt.Errorf("ensure token: %w", err)
	}

	// The listen port to suggest in the server URL / placeholder.
	port := "6464"
	if _, p, perr := net.SplitHostPort(listen); perr == nil && p != "" {
		port = p
	}

	server := ""
	advertised := advertise != ""
	if advertised {
		host := advertise
		// Bare host (no :port): append the listen port so the URL is complete.
		if !strings.Contains(host, ":") {
			host = net.JoinHostPort(host, port)
		}
		server = "https://" + host
	} else {
		// No --advertise: the VM can't know its own public IP. Leave a filled-in
		// placeholder so the operator sees exactly what to replace.
		server = fmt.Sprintf("https://REPLACE_WITH_VM_PUBLIC_IP:%s", port)
	}

	cfg := &config.Config{
		CurrentContext: "default",
		Contexts: map[string]config.Context{
			"default": {
				Token:         token,
				Server:        server,
				CAFingerprint: fingerprint,
				Space:         space,
			},
		},
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("marshal node config: %w", err)
	}

	// A header comment tells the operator how to use the file and, when server
	// is a placeholder, exactly what to set. yaml.Marshal can't emit comments,
	// so we prepend them to the document.
	var header string
	if advertised {
		header = "# sklp node config — one deploy context per VM. Copy to your workstation and use with\n" +
			"# `XDG_CONFIG_HOME=<dir> sklp deploy ls` (place it as <dir>/sklp/config.yaml),\n" +
			"# or merge the context into your ~/.config/sklp/config.yaml.\n"
	} else {
		header = "# sklp node config — one deploy context per VM. Copy to your workstation and use with\n" +
			"# `XDG_CONFIG_HOME=<dir> sklp deploy ls` (place it as <dir>/sklp/config.yaml).\n" +
			"# TODO: replace REPLACE_WITH_VM_PUBLIC_IP in `server` with this VM's public\n" +
			"# address (the agent can't know its own public IP; pass --advertise to fill it).\n"
	}
	out := append([]byte(header), data...)

	// Written beside the token/cert, NOT over the operator's own config.yaml.
	path := filepath.Join(filepath.Dir(control.TokenPath(space)), "node-config.yaml")
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return "", fmt.Errorf("write node config: %w", err)
	}
	return path, nil
}

// shipperSet owns one logship.Shipper per space this node supervises. The
// node-mode listener is registry-backed and shared across spaces, but logship
// is 1 Shipper : 1 space (see its package doc), so the shippers are tracked
// here rather than started once for the whole agent.
type shipperSet struct {
	mu    sync.Mutex
	stops map[string]func()
}

func newShipperSet() *shipperSet { return &shipperSet{stops: map[string]func(){}} }

// start ships space's logs, replacing any shipper a previous registration of
// the same name left behind — control.Registry.Register replaces a handle in
// place, so a re-registered space would otherwise leak a Shipper still tailing
// the dead Supervisor's sources.
func (s *shipperSet) start(ctx context.Context, space, node string, sup control.Supervisor) {
	if !logship.Configured() {
		return
	}
	_, stop, err := logship.Start(ctx, space, node, logshipSupervisor{sup})
	if err != nil {
		fmt.Fprintf(os.Stderr, "sklet agent: log shipping disabled for space %q: %v\n", space, err)
		return
	}
	s.mu.Lock()
	prev := s.stops[space]
	s.stops[space] = stop
	s.mu.Unlock()
	if prev != nil {
		prev()
	}
}

func (s *shipperSet) stop(space string) {
	s.mu.Lock()
	stop := s.stops[space]
	delete(s.stops, space)
	s.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// selfPath names this binary for an operator message, falling back to the
// command name when the executable cannot be resolved.
func selfPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "sklet"
	}
	return exe
}

const imageGCInterval = time.Hour

func collectImagesEvery(ctx context.Context, every time.Duration) {
	control.CollectImages()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			control.CollectImages()
		}
	}
}
