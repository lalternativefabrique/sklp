package space

import (
	"fmt"
	"sort"

	"github.com/lalternativefabrique/sklp/space/ociproc"
)

// Service is the runtime's view of a pod-like sidecar. It is kept
// independent of runner.Service so the runtime package never imports
// runner (which already imports runtime — the reverse would be a
// cycle). The caller (runner/spawn) maps schema.Service into this shape
// via ServiceFromSchema.
//
// Network sharing model (this iteration): every service in a run shares
// ONE network namespace with the runner. There is no bridge and no DNS
// by name — services reach each other (and the runner reaches them) on
// 127.0.0.1:<port>. The Ports field is therefore not honoured by the
// embedded backend yet (everything is on loopback); it is carried so the
// mapping stays loss-less and a future bridge backend can use it.
type Service struct {
	// Name is the service key from the pipeline (e.g. "db"). Used for
	// container id, bundle dir, and log prefixes.
	Name string

	// Image is the OCI reference to pull and run.
	Image string

	// Run is a shell command for an image-less service. It executes on the
	// space's runner rootfs (which bakes the space.yaml toolchain) via
	// `sh -c <Run>`, mutually exclusive with Image. The CI-via-Space path
	// reuses the runner rootfs so a `run:` service sees the same tools the
	// runner does (e.g. a `migrate` step reaching `postgres:5432`).
	Run string

	// Command overrides the image default CMD. Empty = image default. By
	// default the image ENTRYPOINT is preserved and Command is appended
	// (Docker semantics): nats entrypoint [nats-server] + command [-js] runs
	// `nats-server -js`. Set IgnoreEntrypoint to run Command verbatim.
	Command []string

	// IgnoreEntrypoint runs Command as the full argv, ignoring the image
	// ENTRYPOINT (Docker's --entrypoint). The dev run: path sets it so a
	// toolchain image whose entrypoint is `sklp` still runs `sh -c <cmd>`.
	IgnoreEntrypoint bool
	// NestedRuntime grants CAP_SYS_ADMIN inside the service's own user namespace
	// so runc can run containers within it (the CI runner builds images).
	NestedRuntime bool

	// ImageUID/ImageGID are the image's own USER, as the OCI config declares
	// it. An image that asks to run as a non-root uid gets it: some react to
	// finding themselves root by discarding the config they were built with
	// (nginx-unprivileged falls back to its packaged one, which binds :80).
	// A symbolic username is resolved against the image's own etc/passwd;
	// zero means root, the OCI default.
	ImageUID int
	ImageGID int

	// PauseInit parks the container init on `sklp pause` instead of Command,
	// for start-detached-then-runc-exec parking. Overrides Command; the init
	// becomes sklp's own static binary, so parking never needs the guest's
	// `sleep`/`sh`. See buildServiceSpecWithMappings.
	PauseInit bool

	// Env is the process environment (KEY=value entries).
	Env []string

	// DependsOn names other services that must reach Ready before this
	// one is launched.
	DependsOn []string

	// Oneshot marks a run-to-completion service: exit 0 = ready (and
	// unblocks dependents), exit != 0 fails the whole run. Ready is
	// ignored for oneshot services.
	Oneshot bool

	// Ready is the readiness probe. nil = launch-and-forget (the service
	// is considered ready as soon as its container init is up).
	Ready *ServiceReady

	// Ports / Volumes / Network are carried from the schema but NOT
	// honoured by the embedded backend in this iteration (shared-netns /
	// loopback only). Documented in the package and on Service.
	Ports   []string
	Volumes []string
	Network string

	// DevMounts is the OPT-IN host bind-mount set. It is filled ONLY by
	// the `sklp dev` devstack path (repo hot-reload); the deploy and CI
	// paths leave it nil so buildServiceSpec emits the exact, host-bind-free
	// spec they have today (the security frontier: deploy never bind-mounts
	// the host). See DevMount.
	DevMounts []DevMount

	// CgroupsPath is the runtime-spec linux.cgroupsPath the launcher resolved
	// for this container (Space.Start) when Resources asks for a cap; empty
	// leaves the spec without one.
	CgroupsPath string

	// Resources caps the container's cgroups v2 controllers (cpu/memory/pids).
	// The zero value leaves the service unbounded.
	Resources ociproc.Limits

	// RequireResources fails the launch when Resources asks for a cap the host
	// cannot apply (no cgroups v2 delegation), instead of running unbounded.
	RequireResources bool

	// HostNetwork, when true, omits the container's private network namespace
	// so it SHARES the host netns and binds host ports directly. Reserved for
	// trusted host-level singletons (the gateway traefik that must own the
	// host's :443/:80). Default false = private netns. See buildServiceSpec.
	HostNetwork bool

	// Capabilities are granted ON TOP of ociproc.ServiceCapabilities, in OCI
	// spelling. Empty is the norm: the baseline already covers an entrypoint
	// that chowns its data dir and drops to its own user.
	Capabilities []string
}

// DevMount is one host->container bind-mount, used ONLY by `sklp dev`. The
// host repo (or a sub-path) is bound rw so the in-container toolchain sees
// live edits for hot-reload. It is rootless: the container writes as the
// host uid mapped to container uid 0 (the spec's uid mapping), so files it
// creates in the repo stay owned by the operator — never root. ReadOnly
// flips it to a ro bind (e.g. a config the service must not mutate).
type DevMount struct {
	// Source is the absolute HOST path. Must be host-absolute; the caller
	// (devstack) resolves it from the repo root before building the spec.
	Source string
	// Destination is the absolute path inside the container.
	Destination string
	// ReadOnly binds ro instead of rw. Default (false) = rw for hot-reload.
	ReadOnly bool
}

// ServiceReady mirrors runner.Ready without importing it. Exactly one of
// HTTP / TCP / Log / File is honoured, picked in that order.
type ServiceReady struct {
	HTTP    string
	TCP     string
	Log     string
	File    string
	Timeout string // Go duration string; empty = default 30s.
	Soak    string // Go duration string; how long a new generation must stay Ready after cutover. Empty = supervisor default.
}

// ServiceFromSchema maps the loss-less primitives of a runner.Service
// into the runtime's internal Service. It takes the fields directly
// (rather than the schema type) so the runtime stays decoupled from the
// runner package. The caller in runner/spawn does the field copy.
func ServiceFromSchema(name, image, run string, command, env, dependsOn []string, oneshot bool, ready *ServiceReady, ports, volumes []string, network string) Service {
	return Service{
		Name:      name,
		Image:     image,
		Run:       run,
		Command:   command,
		Env:       env,
		DependsOn: dependsOn,
		Oneshot:   oneshot,
		Ready:     ready,
		Ports:     ports,
		Volumes:   volumes,
		Network:   network,
	}
}

// orderServices returns the services topologically sorted so that every
// service appears after all of its DependsOn. Independent services keep a
// stable lexicographic order for deterministic launches. It rejects
// cycles and references to unknown services with a clear error.
//
// DependsOn is validated at pipeline load time, but we re-validate here
// so the runtime is safe to call directly (tests, future callers) without
// relying on an upstream guard.
func orderServices(services []Service) ([]Service, error) {
	byName := make(map[string]Service, len(services))
	for _, s := range services {
		if _, dup := byName[s.Name]; dup {
			return nil, fmt.Errorf("duplicate service name %q", s.Name)
		}
		byName[s.Name] = s
	}
	for _, s := range services {
		for _, dep := range s.DependsOn {
			if _, ok := byName[dep]; !ok {
				return nil, fmt.Errorf("service %q depends on unknown service %q", s.Name, dep)
			}
		}
	}

	const (
		white = 0 // unvisited
		gray  = 1 // on the current DFS stack
		black = 2 // fully processed
	)
	color := make(map[string]int, len(services))
	var ordered []Service

	// Stable iteration: sort names once, recurse depth-first. Visiting
	// dependencies before the node yields dependency-first order.
	names := make([]string, 0, len(services))
	for _, s := range services {
		names = append(names, s.Name)
	}
	sort.Strings(names)

	var visit func(name string, stack []string) error
	visit = func(name string, stack []string) error {
		switch color[name] {
		case black:
			return nil
		case gray:
			return fmt.Errorf("service dependency cycle: %s -> %s", join(stack), name)
		}
		color[name] = gray
		deps := append([]string(nil), byName[name].DependsOn...)
		sort.Strings(deps)
		for _, dep := range deps {
			if err := visit(dep, append(stack, name)); err != nil {
				return err
			}
		}
		color[name] = black
		ordered = append(ordered, byName[name])
		return nil
	}

	for _, name := range names {
		if err := visit(name, nil); err != nil {
			return nil, err
		}
	}
	return ordered, nil
}

// join renders a dependency stack for cycle error messages.
func join(stack []string) string {
	out := ""
	for i, s := range stack {
		if i > 0 {
			out += " -> "
		}
		out += s
	}
	return out
}
