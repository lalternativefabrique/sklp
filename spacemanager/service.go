// Package deploy is the single-node, daemonless reconciliation core of
// `sklp deploy`. It runs N long-lived services, detects their death via
// pidfd, reaps the dead container inits (subreaper, so no zombie keeps a
// stale PID alive — the failure mode the spike proved), and relaunches
// them with capped exponential backoff until the actual state matches the
// desired state.
//
// Scope through milestone 3 (the core, nothing else): the desired state is
// handed in as Go values; every external dependency — the launcher, the
// readiness checker, the reverse-proxy controller — is injected so the loop
// is testable with plain processes (the exec launcher), fake probes, and a
// fake proxy, without runc privileges, a network, or a real traefik. Still
// out of scope: deploy.yaml parsing, the remote CLI (milestone 4), TLS,
// and multi-node.
package spacemanager

import (
	"slices"

	"github.com/lalternativefabrique/sklp/space"
)

// Probe is the readiness probe for a service. We reuse space.ServiceReady
// directly (both packages are embedded_runtime-gated, so the import compiles)
// rather than redefining a parallel type and mapping it: zero duplication, one
// source of truth for the probe shape, and space.WaitServiceReady consumes
// the very same value. (Aliased here so the field type reads as deploy.Probe.)
type Probe = space.ServiceReady

// Service is the desired state of one managed service, described by an image,
// an optional command override, and a process env. Replicas, Port and Proxy
// are plain FIELDS, not new kinds (the "minimal YAML core" rule): N replicas,
// a routed port, and a host/path are properties of the same Service.
type Service struct {
	// Name uniquely identifies the service in the desired set. It is the base
	// of every instance id (<Name>-0, <Name>-1, ...) and appears in logs.
	Name string

	// Image is the OCI reference the runc launcher pulls and runs. The exec
	// launcher (tests) ignores it.
	Image string

	// DeclaredImage is the image the last full apply declared for this
	// service. A rolled image diverges from it; a rollback keeps it.
	DeclaredImage string `json:",omitempty"`

	// RolledImages are the tags image rolls moved this service through since
	// DeclaredImage was recorded. A full apply declaring one of them is a
	// manifest the rolls already passed; the running image is kept.
	RolledImages []string `json:",omitempty"`

	// Command overrides the image default. For the exec launcher it IS the
	// argv. Empty = image default (runc launcher only).
	Command []string

	// Env is the process environment (KEY=value entries).
	Env []string

	// Ready is the readiness probe. nil = launch-and-forget: the service is
	// considered ready as soon as its init is up (milestone-1 behaviour, no
	// gate). A non-nil probe gates the service: it is "ready" (and counted
	// satisfied) only after the probe passes.
	Ready *Probe

	// Resources caps the service's cgroups v2 controllers. The zero value
	// still carries the default pids cap (see defaultServicePidsMax).
	Resources ServiceResources

	// Replicas is the desired number of running instances of this service.
	// 0 or 1 mean a single instance; N>1 means N independent instances, each
	// with its own pid/pidfd/state/backoff (milestones 1-2 applied per
	// instance). Reconciliation targets "Replicas instances Ready".
	//
	// PORT/ADDRESSING: the N instances do NOT each bind a host port (that
	// would collide). Replicas are reachable only through the proxy, which
	// addresses each instance by the internal address the Launcher reports
	// via Addr(instanceID). With the exec launcher (tests) that address is a
	// synthetic per-instance string; with runc it is the container's
	// per-instance IP:port. No host-port fan-out is attempted here — that is a
	// network concern deliberately left to the proxy.
	Replicas int

	// Port is the application port instances listen on; 0 = not fronted by the
	// proxy. It is the port the proxy targets on each instance address.
	Port int

	// Proxy, when non-empty, is the routing rule the proxy matches to send
	// traffic to this service's Ready instances. Free-form for this milestone
	// (e.g. a host "api.example.com" or a path prefix "/api"); the proxy
	// controller interprets it. Empty with a non-zero Port = route by
	// service name (default backend), still load-balanced across replicas.
	Proxy string

	// Ports are docker-style host ingress mappings ("8080:80"): each binds a
	// HOST port (127.0.0.1:<host>) whose TCP traffic the agent proxies into the
	// space netns, round-robin across this service's Ready replicas. Empty =
	// no host ingress (the service is reachable only east-west by name).
	Ports []string

	// ProxyProtocol lists the container ports of Ports whose relayed connections
	// carry a PROXY protocol v2 header.
	ProxyProtocol []int

	// Volumes are the declared `volumes:` entries (docker -v form: a named
	// "name:/ctr" or a host bind "/host:/ctr"). The runc launcher resolves each
	// to an rw bind-mount — REQUIRED since the service rootfs is read-only, so a
	// service's data path (postgres PGDATA, nats store) writes to its volume,
	// not the image. Named volumes are scoped per space and persist across runs.
	Volumes []string

	// Capabilities the service needs beyond the hardened baseline.
	Capabilities []string

	// DependsOn names the services/jobs this one waits on before it may start:
	// an upstream service must be Ready, an upstream job must be Done (exit 0).
	// Pure ORDERING — it never alters the runnable definition, so it is excluded
	// from specChanged/serviceDiffers (changing depends_on does not roll a
	// container). The reconciler (a later layer) consumes it; here it is carried.
	DependsOn []string

	// Oneshot marks a JOB: a run-once-to-completion task (migration, backup)
	// rather than a long-lived service. A `jobs:` entry resolves to a Service
	// with Oneshot=true, a `services:` entry to Oneshot=false. The reconciler (a
	// later layer) treats a clean exit of a Oneshot as terminal (Done), not a
	// crash; for ordering, a Done job satisfies its dependents. Not part of the
	// runnable-definition diff (it is a kind, not a mutable field).
	Oneshot bool

	// Backup, when set, schedules a recurring backup command the supervisor runs
	// inside a live instance of this service. Carried, not part of the runnable
	// diff (changing the schedule never rolls the container).
	Backup *ServiceBackup

	// Schedule, on a Oneshot, is the 5-field cron the supervisor re-runs the job
	// on. Empty means run-once (the default). Unlike Backup — which execs into an
	// already-running service — this relaunches the job's own container, so the
	// recurring work gets a fresh filesystem and its own image. Carried, not part
	// of the runnable diff.
	Schedule string
}

// ServiceBackup is a resolved backup directive on a Service: the command to run
// inside a live instance and the cron schedule the supervisor fires it on.
type ServiceBackup struct {
	Command  []string
	Schedule string
}

// proxied reports whether the service is fronted by the reverse proxy: it has
// an application port to target. Only proxied services drive SetBackends.
func (s Service) proxied() bool { return s.Port != 0 }

// HasProxiedService reports whether any resolved service is fronted by the
// reverse proxy (Port set). The supervisor uses it to decide whether to wire
// a traefik proxy when it only has the resolved services (no DeployConfig), as
// at boot under systemd where the cwd / deploy.yaml is unavailable.
func HasProxiedService(services []Service) bool {
	for _, s := range services {
		if s.proxied() {
			return true
		}
	}
	return false
}

// HasGatewayRoute reports whether a space declares any public gateway route
// (is fronted by the host gateway). The supervisor uses it to decide whether to
// bring up the gateway driver at all — when a space has no route, no front door
// is opened. Mirrors HasProxiedService for the gateway path.
func HasGatewayRoute(meta SpaceMeta) bool {
	return len(meta.Routes) > 0
}

// replicaCount is the desired instance count, normalising 0 to 1 (a service is
// always at least one instance unless explicitly scaled out).
// singleWriter: a service that mounts a volume writes it — postgres, nats,
// redis, a backup job — and such stores tolerate one writer, so two of its
// instances must never overlap; the price is the stop-to-ready gap at cutover.
func (s Service) singleWriter() bool { return len(s.Volumes) > 0 }

func (s Service) replicaCount() int {
	if s.Replicas < 1 {
		return 1
	}
	return s.Replicas
}

// specChanged reports whether the runnable definition (what actually requires
// a fresh container) differs between two specs. Replicas/Port/Proxy are
// excluded: scaling or re-routing does not require replacing a running
// instance, only a rolling replacement of image/command/env does.
func specChanged(a, b Service) bool {
	if a.Image != b.Image {
		return true
	}
	if !equalStrings(a.Command, b.Command) {
		return true
	}
	if !equalStrings(a.Capabilities, b.Capabilities) {
		// Same reason as Resources: capabilities live in the OCI spec, read once
		// at create. Accepting a change that never applies would report a service
		// as hardened while it kept the set it booted with.
		return true
	}
	if a.Resources != b.Resources {
		// Resources land in the OCI spec, which runc reads once at create: a
		// changed cap only takes effect on a fresh container. Comparing it here
		// rolls the service; leaving it out would report the apply as applied
		// while the old cap stayed in force.
		return true
	}
	if !equalStrings(a.Volumes, b.Volumes) {
		// Same reason as Resources/Capabilities: mounts are baked into the OCI
		// spec at create. Without this, a changed bind-mount source or
		// destination reports "unchanged" and the container keeps running
		// against the stale mount forever — only a full agent restart (which
		// rebuilds every SpaceOptions from the manifest) ever picked it up.
		return true
	}
	if !equalProbe(a.Ready, b.Ready) {
		// The agent judges an instance by the probe it started it with. A
		// corrected probe (a log line the image never prints) left out of the
		// diff reported "unchanged" while the service stayed starting forever,
		// relaunched each time the old probe timed out.
		return true
	}
	return !equalStrings(a.Env, b.Env)
}

func equalProbe(a, b *Probe) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// serviceDiffers reports whether two specs of the same service differ in ANY
// reconcilable field: the runnable definition (specChanged) OR the scale/route
// knobs (replicas/port/proxy/ports). It is the diff POST /v1/apply uses to
// decide unchanged-vs-configured for a present service, where specChanged alone
// would miss a pure scale or re-route. Names are assumed equal (same service).
// SameSpec reports whether two specs of the same service would apply as
// unchanged. DeclaredImage is bookkeeping, not part of the runnable spec.
func SameSpec(a, b Service) bool { return !serviceDiffers(a, b) }

const maxRolledImages = 64

// JournalRolledImage appends image to the roll journal, dropping the oldest
// entries past maxRolledImages.
func JournalRolledImage(journal []string, image string) []string {
	if slices.Contains(journal, image) {
		return journal
	}
	out := append(append([]string(nil), journal...), image)
	if len(out) > maxRolledImages {
		out = out[len(out)-maxRolledImages:]
	}
	return out
}

func serviceDiffers(a, b Service) bool {
	if specChanged(a, b) {
		return true
	}
	if a.replicaCount() != b.replicaCount() {
		return true
	}
	if a.Port != b.Port || a.Proxy != b.Proxy {
		return true
	}
	return !equalStrings(a.Ports, b.Ports) || !slices.Equal(a.ProxyProtocol, b.ProxyProtocol)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
