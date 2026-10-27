package space

import (
	"context"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	rtembed "github.com/lalternativefabrique/sklp/space/embed"
	"github.com/lalternativefabrique/sklp/space/ociproc"
)

// DeployService is the minimal description `sklp deploy` hands to the runc
// space: a standalone long-lived container (its own namespaces, no shared
// netns, no readiness gate — those are later milestones). It is deliberately
// narrower than the pod-oriented Service used by the CI multi-container path.
type DeployService struct {
	Name    string
	Image   string
	Command []string
	Env     []string

	// Port is the port the service listens on inside its container (the deploy
	// `port:`). Recorded so a bare-path HTTP readiness probe is built into a
	// dialable http://<containerIP>:<Port><path>. 0 = unset.
	Port int

	// Capabilities granted on top of the hardened service baseline.
	Capabilities []string

	// IgnoreEntrypoint runs Command verbatim, discarding the image ENTRYPOINT
	// (Docker --entrypoint). `sklp deploy` leaves it false (entrypoint kept,
	// so a `command:` of args like nats' [-js] runs under nats-server). The dev
	// run: path sets it so the toolchain image's `sklp` entrypoint is bypassed
	// for `sh -c <cmd>`.
	IgnoreEntrypoint bool
	NestedRuntime    bool

	// PauseInit parks the container init on `sklp pause` (see spec.go) instead
	// of a Command, for the "start detached, then runc exec the real work in"
	// pattern (CI runner, dev space, `sklp shell`). It overrides Command and
	// makes the init sklp's own static binary — never the guest's `sleep`/`sh`,
	// which a minimal space.yaml rootfs does not ship.
	PauseInit bool

	// WorkdirOverride, when non-empty, replaces the image's WorkingDir as the
	// container cwd. `sklp deploy` leaves it "" (image default); `sklp dev`
	// sets it to the in-container mount point of the repo so a `run:` command
	// like `pnpm dev` executes in the bind-mounted source tree.
	WorkdirOverride string

	// DevMounts is the OPT-IN host bind-mount set, filled ONLY by `sklp dev`
	// (repo hot-reload). `sklp deploy` leaves it nil — the security frontier:
	// a deployed space never bind-mounts the host. When nil the launched
	// spec is byte-identical to the deploy spec. See runtime.DevMount.
	DevMounts []DevMount

	// Volumes are the declared `volumes:` entries (docker -v form: a named
	// "name:/ctr" or a host bind "/host:/ctr"). The space resolves each to
	// an rw bind-mount — REQUIRED since the rootfs is read-only, so a service's
	// data path (postgres PGDATA, nats store) writes to its volume, not the
	// image. Named volumes persist via the space (see volumesRoot).
	Volumes []string

	// Resources caps the service's cgroups v2 controllers (cpu/memory/pids)
	// from the deploy `resources:` block. Zero leaves the service unbounded.
	Resources ociproc.Limits

	// RequireResources fails the launch when Resources cannot be applied (no
	// cgroups v2 delegation) instead of silently running unbounded.
	RequireResources bool

	// HostNetwork, when true, makes the container SHARE THE HOST network
	// namespace and bind host ports directly, instead of getting a private
	// netns. Reserved for trusted host-level singletons like the gateway
	// traefik that must own the host's :443/:80. Default false = private netns.
	HostNetwork bool

	// RootfsDir, when non-empty, is a pre-built rootfs directory the launcher
	// uses VERBATIM instead of pulling+extracting Image. `sklp dev` sets it to a
	// runner rootfs built in-process (runtime/runnerbuild) so a run: service that
	// needs baked packages/toolchain runs on it without a registry image. Image
	// then names the recipe only (for logs); the OCI config (Entrypoint/Cmd/Env/
	// WorkingDir) comes from RootfsCfg, not a pulled manifest.
	RootfsDir string

	// RootfsCfg is the OCI image config to pair with RootfsDir (Env, WorkingDir;
	// Entrypoint is bypassed for run: via IgnoreEntrypoint). Ignored when
	// RootfsDir is empty.
	RootfsCfg ImageConfig
}

// ImageConfig is the exported view of the OCI image config a caller can supply
// alongside a pre-built RootfsDir. Mirrors the fields the bundle writer reads.
type ImageConfig struct {
	Env        []string
	WorkingDir string
}

// Space is sklp's containerization unit, live: the runc-backed aggregate that
// owns one space's IDENTITY (its name + scoping, via the embedded scope), its
// NETWORK (the lazily-built spaceNet bridge + host ingress), and its SERVICES
// (the container pids/ips it tracks). One Space serves many services across the
// agent's lifetime; deploy and dev each drive a Space per space they run.
//
// It is the execution aggregate the domain doc calls "Space" (the routing
// IDENTITY — ephemeral/URL/TTL — stays a deploy-side SpaceMeta layered above).
// The deploy/dev packages hold a *Space and call Start/Stop/Close/OpenIngress;
// the CI single/multi-container path uses a standalone (empty-name) Space with
// no bridge and the legacy global paths, byte-identical to before.
type Space struct {
	cacheRoot string
	runcBin   string
	runcRoot  string
	slirpBin  string

	// selfExe is the path to this sklp binary, used by the paired OCI hook
	// programs: runc re-execs it to attach a space service's network BEFORE
	// its command runs. Empty if it could not be resolved — the space then
	// falls back to the post-start host-side attach.
	selfExe string

	// scope is this space's identity + path/id scoping: name + the single
	// source of every host path + runc id derived from the name (volumes, runc
	// state, container ids). An empty-name scope is the standalone space (no
	// bridge, global paths) — the pre-spacenet behaviour.
	scope spaceScope

	// mu guards the lazily-built space network and the pid bookkeeping. The
	// space net is created ONCE (on the first Start) and shared by every
	// service; pids maps the space service id to the container init pid so
	// Stop can detach it from the bridge.
	mu   sync.Mutex
	cnet *spaceNet
	// cnetErr/cnetErrAt cache a creation failure for cnetErrTTL: a transient
	// failure (e.g. a newuidmap race under concurrent space starts — two
	// spaces on one node materialising their netns workers at once) must not
	// become permanent for the space's whole lifetime. Without a TTL, every
	// later Start (including the backoff-driven retries reconcile() already
	// schedules per instance) hit this same cached error forever, wedging the
	// space's services in Pending with no further log output — the retry loop
	// LOOKED like it was working (the log said "retrying in 1s") but never
	// actually attempted spaceNet() creation again. After cnetErrTTL elapses,
	// the next Start retries creation from scratch.
	cnetErr   error
	cnetErrAt time.Time
	ingress   *hostIngress // host->space TCP ingress; built with the space net
	pids      map[string]int
	stopGrace time.Duration
	// ips maps a space service id to its bridge IP, recorded at Attach so a
	// readiness probe dials the container natively from the space netns.
	ips map[string]string
	// ports maps a space service id to the port it listens on (the deploy
	// `port:`), so a bare-path HTTP readiness probe ("/health") can be built
	// into a dialable http://<ip>:<port>/health. 0 = unknown.
	ports map[string]int
	// gwPorts memoises the loopback host port allocated to front a service for
	// the host GATEWAY (OpenGatewayIngress), keyed by service name. It keeps the
	// call idempotent — a second resolution returns the same port — and is
	// SEPARATE from explicit user ingress (Service.Ports), so the two never
	// collide on one allocation. Reset on Close with the rest of the net state.
	gwPorts map[string]int
	// allowHost re-opens the host's own LAN /32s through the RFC1918 egress
	// blackhole. Set ONLY by `sklp dev` (AllowHostAccess); deploy and CI leave
	// it false, so their filter is unchanged. It is a setter rather than a
	// NewSpace parameter on purpose: the default must stay closed without any
	// caller having to pass a value for it.
	allowHost bool

	// allowEgress is the space's Internet allowlist (CIDRs). Empty leaves egress
	// open; a non-empty list makes the space's outbound traffic default-deny.
	allowEgress []string

	// overlayHolders maps a service name to its live overlay-holder pid. A
	// supervisor restart of a crashed service calls Start again; without killing
	// the prior holder the new one would mount an overlay whose upper/work are
	// still held by the old holder's live overlay → EBUSY. Killing it first
	// releases the overlay so the relaunch gets a clean mount. Keyed by service
	// name (not instance id) — one holder per service instance.
	overlayHolders map[string]int

	// childUserns puts every service of this space in a user namespace that is
	// a child of the space's, so container root is a subuid instead of the
	// operator (UseChildUserns). childIdent is that identity once the first
	// holder exists; childRootID is its root as the space maps it.
	childUserns bool
	childIdent  ociproc.Identity
	childRootID int
}

// UseChildUserns runs this space's services as a root of their own, a subuid
// that owns nothing of the operator's (see spacenet.SpawnOverlayChild). The
// node's deploy launcher decides it; a dev or CI space never does, since it
// has to be the operator to write the repository it mounts.
func (s *Space) UseChildUserns() error {
	if s.scope.standalone() {
		return fmt.Errorf("a standalone space has no child user namespace")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.childUserns = true
	return nil
}

// NewSpace resolves the runc binary and runc state root once
// (so each Start only prepares the image and bundle) and scopes the space to
// a space: every service it launches joins that space's shared bridge
// network (built once, lazily, on the first Start) so siblings reach each other
// by service name. An empty name yields standalone services (no space bridge,
// the pre-spacenet behaviour).
func NewSpace(spaceName string) (*Space, error) {
	root, err := cacheRoot()
	if err != nil {
		return nil, fmt.Errorf("deploy space: cache root: %w", err)
	}
	runcBin, err := resolveRunc(root)
	if err != nil {
		return nil, fmt.Errorf("deploy space: runc: %w", err)
	}
	// runc state + container ids are SCOPED PER SPACE: a named space gets its
	// own state root + id prefix, so a same-named service in two spaces never
	// collides on one runc id or reaps the other's containers (invariant I4).
	// The standalone space keeps the legacy shared root + global prefix.
	scope := newScope(spaceName, root)
	runcRoot := scope.runcStateRoot()
	if err := os.MkdirAll(runcRoot, 0o755); err != nil {
		return nil, err
	}
	// slirp is best-effort egress for the space; an extraction failure only
	// loses Internet, never the space (newSpaceNet treats "" as no egress).
	slirpBin, _ := rtembed.ExtractSlirp(root)
	// Reap any of THIS space's deploy containers an interrupted prior run left
	// behind (Ctrl-C before teardown, a crash) so a fresh deploy never buts on a
	// stale id still `running`. Scoped to this space's id prefix in this space's
	// state root, so it never touches another space's containers; best-effort.
	reapAllContainers(runcBin, runcRoot, scope.containerIDPrefix())
	// Resolve this binary once for the createRuntime hook path. A failure only
	// disables the hook (the space falls back to the post-start attach); it
	// must never fail space creation.
	selfExe, _ := os.Executable()
	return &Space{
		cacheRoot:      root,
		runcBin:        runcBin,
		runcRoot:       runcRoot,
		slirpBin:       slirpBin,
		selfExe:        selfExe,
		scope:          scope,
		pids:           map[string]int{},
		stopGrace:      stopGracePeriod,
		ips:            map[string]string{},
		ports:          map[string]int{},
		gwPorts:        map[string]int{},
		overlayHolders: map[string]int{},
	}, nil
}

// deployIDPrefix prefixes every deploy container id so orphan cleanup can scope
// a `runc list` reap to this space's containers without touching other runc
// state (e.g. the CI multi-container run path) sharing the binary's caches.
const deployIDPrefix = "sklp-deploy-"

// DeployContainerInfo is one running (or still-in-state) deploy container, as
// seen in the runc state — the source of truth `sklp deploy ps` renders. ID is
// the runc id ("sklp-deploy-<svc>-<i>"); Status is the runc status string.
type DeployContainerInfo struct {
	ID     string
	Status string
}

// ListDeployContainers returns every container OF THE GIVEN SPACE in its runc
// state root, with its runc status. READ-ONLY: it never kills or deletes
// anything (unlike NewSpace, which reaps on start), so
// `sklp deploy ps` can introspect a live deploy without disturbing it. It
// resolves the runc binary + the space's state root the same way the space
// does, so the view matches what that space's deploy created. An empty
// spaceName lists the standalone space (legacy global state root).
func ListDeployContainers(spaceName string) ([]DeployContainerInfo, error) {
	root, err := cacheRoot()
	if err != nil {
		return nil, fmt.Errorf("deploy ps: cache root: %w", err)
	}
	runcBin, err := resolveRunc(root)
	if err != nil {
		return nil, fmt.Errorf("deploy ps: runc: %w", err)
	}
	scope := newScope(spaceName, root)
	runcRoot := scope.runcStateRoot()
	prefix := scope.containerIDPrefix()
	var out []DeployContainerInfo
	for _, id := range listContainers(runcBin, runcRoot) {
		if !strings.HasPrefix(id, prefix) {
			continue
		}
		out = append(out, DeployContainerInfo{ID: id, Status: containerStatus(runcBin, runcRoot, id)})
	}
	return out, nil
}

// DeployInstanceID is the runc id of a deploy instance ("<svc>-<i>") in the
// given space — exported so the CLI can map a resolved instance id onto the
// runc id ListDeployContainers reports, for space-scoped filtering. Scoped per
// space (the prefix folds in the space name) so two spaces' instances are
// distinct. An empty spaceName yields the standalone (legacy) id.
func DeployInstanceID(spaceName, instance string) string {
	return newScope(spaceName, "").containerID(instance)
}

// DeployIDPrefix is the runc-id (and cgroup basename) prefix shared by every
// container of a space — exported so a cleanup pass can tell a cgroup whose
// space still exists from one whose space is gone and which nothing else can
// reach. An empty spaceName yields the standalone (legacy) prefix.
func DeployIDPrefix(spaceName string) string {
	return newScope(spaceName, "").containerIDPrefix()
}

// containerID is the runc id for a managed service in THIS space. Scoped per
// space (the prefix folds in the space name), so a "core" in space A and a
// "core" in space B are distinct runc ids (invariant I4). Stable across
// restarts so a relaunch first clears any stale state under the same id.
func (s *Space) containerID(name string) string {
	return s.scope.containerID(name)
}

// ContainerID exposes the stable runc id for a managed instance. The deploy
// agent uses it as an opaque proxy-backend address until per-container IP
// resolution lands (see launcher_runc.go Addr).
func (s *Space) ContainerID(name string) string { return s.containerID(name) }

// volumesRoot is the cache subdir under which this space's NAMED volumes are
// materialised. It delegates to the Space so the directory is SCOPED PER SPACE:
// a volume "pgdata" in space A and "pgdata" in space B are distinct host dirs
// (invariant I2), letting two spaces run isolated without data collision. The
// standalone space keeps the legacy flat <cache>/volumes layout.
func (s *Space) volumesRoot() string {
	return s.scope.volumesRoot()
}

// Start prepares the service image, writes the service bundle, launches it
// detached via `runc run -d`, and returns the init PID the agent watches via
// pidfd.
//
// When the space is space-scoped, the service JOINS the space user
// namespace while keeping a PRIVATE network namespace, binds the space
// resolv.conf at /etc/resolv.conf, and is ATTACHED to the space bridge under
// its SERVICE name (the instance id's "<svc>-<i>" suffix is stripped, so all
// replicas register one name for round-robin DNS). Otherwise the service is
// standalone (its own namespaces, no bridge).
//
// NETWORK BEFORE EXEC: createRuntime moves the veth into the container netns,
// then createContainer configures it from inside that namespace. runc invokes
// both before the service command execs, so the network is ready at startup.
// This closes the
// oneshot race: a short-lived service (migrate, nats-init) that needs the
// network and exits fast no longer dies before a post-start attach can run — it
// could not even be attached then, because its pid/netns was already gone. The
// host pre-allocates the IP + veth names (PlanAttach) before launch and embeds
// them in the hook env; the hook fills the pid from the OCI state runc pipes it.
//
// FALLBACK: when the worker's attach socket is unavailable (older space, tmp
// dir unusable), the space reverts to the legacy post-start host-side attach
// — correct for long-lived services, racy only for fast oneshots (the case the
// hook fixes).
func (s *Space) Start(ctx context.Context, svc DeployService) (int, error) {
	var rootfs string
	var imgCfg imageConfig
	if svc.RootfsDir != "" {
		// Pre-built rootfs (sklp dev runner): use it verbatim, no pull. The OCI
		// config comes from the caller, not a manifest.
		rootfs = svc.RootfsDir
		imgCfg.Config.Env = svc.RootfsCfg.Env
		imgCfg.Config.WorkingDir = svc.RootfsCfg.WorkingDir
	} else {
		var err error
		rootfs, imgCfg, err = prepareServiceImage(ctx, s.cacheRoot, svc.Image, runtime.GOARCH)
		if err != nil {
			return 0, fmt.Errorf("deploy space: service %q image: %w", svc.Name, err)
		}
	}
	// The pristine image tree: the overlay's merged path below is mounted in
	// the holder's mount namespace and is invisible from here.
	imageTree := rootfs

	cnet, nerr := s.spaceNet(ctx)
	if nerr != nil {
		return 0, fmt.Errorf("deploy space: service %q: %w", svc.Name, nerr)
	}

	// Writable rootfs via per-service overlay (space-scoped only — that is where
	// the shared persistent userns lives). lower = the shared digest rootfs (RO,
	// pristine); a holder mounts the overlay in its own mount-ns and runc is
	// nsenter'd into it. rootfs becomes the writable merged; volumes/mounts then
	// target the merged. Standalone services keep the bare RO rootfs.
	var overlay serviceOverlay
	overlayActive := false
	if cnet != nil {
		ov, oerr := s.setupServiceOverlay(cnet, svc.Name, rootfs)
		if oerr != nil {
			return 0, fmt.Errorf("deploy space: service %q overlay: %w", svc.Name, oerr)
		}
		overlay = ov
		rootfs = ov.mergedPath
		overlayActive = true
	}

	bundle := s.bundleDir(svc.Name)
	if err := mkdirBundle(bundle); err != nil {
		return 0, err
	}
	if s.childUserns && cnet != nil {
		if err := s.prepareChildBundle(cnet, bundle); err != nil {
			return 0, fmt.Errorf("deploy space: service %q bundle: %w", svc.Name, err)
		}
	}
	logPath := s.LogPath(svc.Name)

	var join *joinNamespaces
	var resolv, hosts string
	var plan *attachPlan
	var hook *createRuntimeHook
	if cnet != nil {
		join = cnet.join()
		resolv = cnet.resolvConf()
		hosts = cnet.hostsConf()
		// Prefer network-before-exec via paired OCI hooks. Reserve the IP + veth
		// names now and wire the hooks into the spec so runc attaches the
		// network before the command runs. selfExe must be known for the hook.
		if s.selfExe != "" && cnet.hookAttachReady() {
			p, perr := cnet.planAttach(serviceNameOf(svc.Name))
			if perr != nil {
				return 0, fmt.Errorf("deploy space: service %q plan attach: %w", svc.Name, perr)
			}
			plan = p
			hook = &createRuntimeHook{
				Path:         s.selfExe,
				MoveEnv:      plan.moveHookEnv(),
				ConfigureEnv: plan.configureHookEnv(),
			}
		}
	}

	ident := serviceIdentity(cnet)
	if s.childUserns && cnet != nil {
		ident = s.childIdent
	}
	containerUID, containerGID, knownUser := imageUser(imageTree, imgCfg.Config.User)
	if !knownUser {
		fmt.Fprintf(os.Stderr, "[sklp deploy] service %q: image USER %q is not in the image's etc/passwd, running as root\n", svc.Name, imgCfg.Config.User)
	}

	// Declared volumes resolve to rw bind-mounts, folded in alongside any dev
	// mounts. A bad volume spec fails the launch loudly rather than letting the
	// service write its data into the read-only rootfs.
	mounts := svc.DevMounts
	if vols, verr := resolveVolumes(svc.Volumes, s.volumesRoot()); verr != nil {
		if plan != nil {
			cnet.releaseAttach(plan)
		}
		return 0, fmt.Errorf("deploy space: service %q volumes: %w", svc.Name, verr)
	} else {
		mounts = append(mounts, vols...)
	}

	// Image VOLUME directives (e.g. postgres' /var/lib/postgresql/data) need a
	// writable backing the same way an explicit volumes: entry does — the rootfs
	// is read-only, so an unmapped image VOLUME would crash the entrypoint's
	// chown/chmod. Docker auto-mounts an anonymous volume for each; mirror that
	// with a per-space emptyDir for any image VOLUME not already covered by a
	// declared mount.
	if anon, aerr := anonImageVolumes(svc.Name, imgCfg.Config.Volumes, mounts, s.volumesRoot()); aerr != nil {
		if plan != nil {
			cnet.releaseAttach(plan)
		}
		return 0, fmt.Errorf("deploy space: service %q image volumes: %w", svc.Name, aerr)
	} else {
		mounts = append(mounts, anon...)
	}
	s.handNewVolumes(cnet, ident, svc.Name, mounts, containerUID, imageTree)

	id := s.containerID(svc.Name)

	limits, err := resolveServiceLimits(svc, id)
	if err == nil && s.childUserns && limits.cgroupsPath != "" {
		// The child root can neither talk to the operator's systemd nor write
		// the operator's cgroup subtree, so runc is told nothing about cgroups;
		// the operator places the container in one itself once it has started
		// (applyChildLimits).
		limits = serviceLimits{}
	}
	if err != nil {
		if plan != nil {
			cnet.releaseAttach(plan)
		}
		return 0, fmt.Errorf("deploy space: service %q resources: %w", svc.Name, err)
	}

	rtSvc := Service{Name: svc.Name, Image: svc.Image, Command: svc.Command, Env: svc.Env, DevMounts: mounts, IgnoreEntrypoint: svc.IgnoreEntrypoint, NestedRuntime: svc.NestedRuntime, HostNetwork: svc.HostNetwork, PauseInit: svc.PauseInit, Capabilities: svc.Capabilities, Resources: limits.applied, CgroupsPath: limits.cgroupsPath, ImageUID: containerUID, ImageGID: containerGID}
	workdir := imgCfg.Config.WorkingDir
	if svc.WorkdirOverride != "" {
		workdir = svc.WorkdirOverride
	}
	if err := writeServiceBundleNet(bundle, rootfs, rtSvc,
		imgCfg.Config.Entrypoint, imgCfg.Config.Cmd, imgCfg.Config.Env,
		workdir, join, resolv, hosts, hook, overlayActive); err != nil {
		if plan != nil {
			cnet.releaseAttach(plan) // reservation never used; reclaim the IP
		}
		return 0, fmt.Errorf("deploy space: service %q bundle: %w", svc.Name, err)
	}

	if s.childUserns && cnet != nil {
		if err := s.handChildBundleFiles(cnet, bundle); err != nil {
			return 0, fmt.Errorf("deploy space: service %q bundle: %w", svc.Name, err)
		}
	}

	if s.childUserns && cnet != nil {
		clearChildCgroup(id)
	}
	_ = deleteContainer(s.runcBin, s.runcRoot, id) // clear a stale prior run

	var beforeStart ociproc.BeforeStart
	if s.childUserns && cnet != nil && svc.Resources.Any() {
		beforeStart = func(pid int) error {
			err := applyChildLimits(id, pid, svc.Resources)
			if err == nil || svc.RequireResources {
				return err
			}
			fmt.Fprintf(os.Stderr, "[sklp deploy] service %q: resource limits not applied: %v\n", svc.Name, err)
			return nil
		}
	}

	// Overlay path: runc must run inside the holder's mount-ns (where the merged
	// is mounted), entered via nsenter. Non-overlay path launches runc directly.
	var pid int
	if overlayActive && s.childUserns && cnet != nil {
		pid, err = startDetachedInNSPidDir(ctx, s.runcBin, s.runcRoot, bundle, id, logPath, limits.systemdCgroup, overlay.holderPID, childRunDir(bundle), beforeStart)
	} else if overlayActive {
		pid, err = startDetachedInNS(ctx, s.runcBin, s.runcRoot, bundle, id, logPath, limits.systemdCgroup, overlay.holderPID, beforeStart)
	} else {
		pid, err = startDetached(ctx, s.runcBin, s.runcRoot, bundle, id, logPath, limits.systemdCgroup, beforeStart)
	}
	if err != nil {
		// With the hook, a network failure already failed the create here (clean:
		// the service never ran unreachable). The hook may still have completed
		// before a later runc failure, so unwind its veth + DNS best-effort too.
		if plan != nil {
			cnet.cancelAttach(plan)
		}
		if leaked := leakedCgroupHint(err, id); leaked != "" {
			return 0, fmt.Errorf("deploy space: start service %q: %w\n%s", svc.Name, err, leaked)
		}
		return 0, fmt.Errorf("deploy space: start service %q: %w", svc.Name, err)
	}

	if cnet != nil {
		var ip string
		if plan != nil {
			// The hooks attached the network during create; only the host-side
			// bookkeeping (for detach + readiness probe) remains.
			cnet.bindAttach(plan, pid)
			ip = plan.ip()
		} else {
			// Fallback: legacy post-start attach (long-lived services only).
			var aerr error
			ip, aerr = cnet.attach(serviceNameOf(svc.Name), pid)
			if aerr != nil {
				// Attach failed: the container is up but unreachable on the bridge.
				// Kill it so the agent retries cleanly rather than leaving a service
				// that can never resolve its siblings.
				killContainer(s.runcBin, s.runcRoot, id)
				return 0, fmt.Errorf("deploy space: service %q space attach: %w", svc.Name, aerr)
			}
		}
		s.mu.Lock()
		s.pids[svc.Name] = pid
		s.ips[svc.Name] = ip
		if svc.Port > 0 {
			s.ports[svc.Name] = svc.Port
		}
		s.mu.Unlock()
	}
	return pid, nil
}

// cnetErrTTL bounds how long a cached space-network creation failure is
// reused before the next Start retries creation from scratch. Short enough
// that a transient race (concurrent newuidmap invocations from two spaces
// starting at once) self-heals within the same backoff window
// reconcile()'s instance-level retry already waits out; long enough that a
// hot retry loop (many instances launching in the same tick) does not
// re-attempt a genuinely broken creation (a missing slirp4netns binary, say)
// dozens of times per second.
const cnetErrTTL = 2 * time.Second

// spaceNet returns the space's network, creating it on first use (or
// re-attempting after a cached failure has aged past cnetErrTTL — see that
// constant). Returns nil when the space is not space-scoped (the standalone
// scope has none by design). A creation failure is the space's failure: a
// service started without the bridge has no name-based DNS and no egress,
// and every later step then dies on a symptom (a DNS lookup, a pull) far
// from this cause — so a fresh error is always returned to the caller
// (never silently swallowed), just not cached forever.
func (s *Space) spaceNet(ctx context.Context) (*spaceNet, error) {
	if s.scope.standalone() {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cnet != nil {
		return s.cnet, nil
	}
	if s.cnetErr != nil && time.Since(s.cnetErrAt) < cnetErrTTL {
		return nil, s.cnetErr
	}
	attachSock := ""
	if s.childUserns {
		// The child root cannot enter the 0700 runtime dir: the socket lives in
		// the space dir instead, and the worker restricts who may connect.
		attachSock = filepath.Join(s.scope.spaceDir(), "attach.sock")
	}
	cn, err := newSpaceNet(ctx, s.scope.Name(), s.slirpBin, s.allowHost, s.allowEgress, attachSock)
	if err != nil {
		s.cnetErr = fmt.Errorf("space network: %w", err)
		s.cnetErrAt = time.Now()
		return nil, s.cnetErr
	}
	s.cnet = cn
	if s.childUserns {
		setRuncEnter(s.runcRoot, cn.enterAsSpaceRoot())
	}
	s.cnetErr = nil
	s.ingress = newHostIngress(cn)
	return s.cnet, nil
}

// AllowHostAccess lets this space's services reach services listening on the
// HOST's own LAN addresses, which the RFC1918 egress blackhole otherwise denies
// (the deny routes stay; a /32 per host address wins longest-prefix match).
//
// It is for `sklp dev` ONLY — a developer reaching their own machine. Deploy
// and CI never call it: on those paths a workload that could dial the host LAN
// is an SSRF pivot, and the CI/build path keeps its fail-closed hook with no
// exception mechanism at all. The cloud metadata endpoint (169.254.0.0/16)
// stays blackholed either way; link-local addresses are never exempted.
//
// Must be called before the first Start: the space network is built once,
// lazily, and a later change is ignored.
func (s *Space) AllowHostAccess() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.allowHost = true
}

// RestrictEgress makes this space's outbound Internet traffic default-deny,
// leaving only allow reachable. An empty list is a no-op, so a space that
// declares nothing keeps the open egress it had. East-west traffic on the space
// bridge is never filtered — services still reach each other by name.
//
// Like AllowHostAccess, it must be called before the first Start: the space
// network is built once, lazily, and a later change is ignored.
func (s *Space) RestrictEgress(allow []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.allowEgress = allow
}

// OpenIngress binds the host listeners for serviceName's port mappings and
// starts proxying host connections into the space (round-robin across the
// service's Ready replicas, dialed in the space netns). serviceName MUST be
// the space DNS service name (what Attach registered, i.e. serviceNameOf the
// instance id) so the worker resolves the right replica set. Idempotent per
// service and a no-op for a standalone space or before the space net
// exists. The agent calls it once a service first becomes Ready.
func (s *Space) OpenIngress(serviceName string, mappings []PortMapping) {
	s.mu.Lock()
	ing := s.ingress
	s.mu.Unlock()
	if ing == nil {
		return
	}
	ing.OpenService(serviceName, mappings)
}

// SendConnReplay splices an accepted host-mux client to serviceName:port in this
// space's netns, replaying prefix (the bytes the mux peeked for the Host header)
// so the backend sees the whole request. It is the route splice the `sklp dev`
// hostname mux calls; *Space satisfies runtime's hostRouter via this method.
// Returns an error (closing nothing) for a standalone space or before the space
// net exists, so the mux closes the client itself.
func (s *Space) SendConnReplay(serviceName string, port int, client net.Conn, prefix []byte) error {
	s.mu.Lock()
	cnet := s.cnet
	s.mu.Unlock()
	if cnet == nil {
		return fmt.Errorf("host mux: space %q has no network (standalone or not yet started)", s.scope.Name())
	}
	return cnet.sendConnReplay(serviceName, port, client, prefix)
}

// SetL7Routes pushes the native L7 ingress routing table (JSON-encoded
// []ingress.Route) to the space worker, which hot-swaps its in-netns reverse
// proxy. "" clears it. A no-op for a standalone space (no network). The agent
// calls it when a space's path/TLS routes change — the native analogue of
// writing the gateway driver's config files.
func (s *Space) SetL7Routes(routesJSON string) error {
	s.mu.Lock()
	cnet := s.cnet
	s.mu.Unlock()
	if cnet == nil {
		return nil
	}
	return cnet.setL7Routes(routesJSON)
}

// SetACLEdges pushes the declared depends_on graph to the space's east-west
// forward-chain ACL. A no-op for a standalone space (no network). The agent
// calls it whenever a service's Ready/dead transition may change what a
// declared edge resolves to.
func (s *Space) SetACLEdges(edges []ACLEdge) error {
	s.mu.Lock()
	cnet := s.cnet
	s.mu.Unlock()
	if cnet == nil {
		return nil
	}
	return cnet.setACLEdges(edges)
}

// OpenGatewayIngress exposes a service to the HOST GATEWAY without a
// user-declared host port: it allocates a free loopback port, opens the SAME
// per-space ingress machinery on it (host 127.0.0.1:<alloc> spliced into the
// space netns, round-robin across Ready replicas dialing containerPort), and
// returns the chosen host port. The gateway dials 127.0.0.1:<hostPort> as the
// service's reachable backend — the bridge that replaces the Addr()=containerID
// stub for a domained service.
//
// Idempotent per service: a second call returns the same allocated port instead
// of binding another listener (the gateway memoises one backend per service).
// The allocation is SEPARATE from explicit user ingress (Service.Ports), so the
// two paths never share a host port. It rides the existing ingress lifecycle —
// Close() tears the listener down with the rest of the space net. Returns an
// error for a standalone space or before the space net exists (no ingress to
// bind), so the caller does not memoise a bogus addr.
func (s *Space) OpenGatewayIngress(serviceName string, containerPort int) (int, error) {
	s.mu.Lock()
	if p, done := s.gwPorts[serviceName]; done {
		s.mu.Unlock()
		return p, nil
	}
	ing := s.ingress
	s.mu.Unlock()
	if ing == nil {
		return 0, fmt.Errorf("gateway ingress: space %q has no network (standalone or not yet started)", s.scope.Name())
	}
	hostPort, err := allocLoopbackPort()
	if err != nil {
		return 0, fmt.Errorf("gateway ingress: service %q: %w", serviceName, err)
	}
	ing.OpenService(serviceName, []PortMapping{{HostPort: hostPort, ContainerPort: containerPort}})
	s.mu.Lock()
	// Re-check under the lock: a racing caller may have allocated first.
	if p, done := s.gwPorts[serviceName]; done {
		s.mu.Unlock()
		return p, nil
	}
	s.gwPorts[serviceName] = hostPort
	s.mu.Unlock()
	return hostPort, nil
}

// GatewayBackend resolves the host-reachable backend address a gateway dials to
// reach a domained service: it opens the gateway ingress on first call (memoised
// thereafter) and returns "127.0.0.1:<hostPort>". The single loopback fronts ALL
// the service's replicas via the space's round-robin, so the gateway needs only
// one backend per service. containerPort is the service's exposed port.
func (s *Space) GatewayBackend(serviceName string, containerPort int) (string, error) {
	hostPort, err := s.OpenGatewayIngress(serviceName, containerPort)
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(ingressBindHost, fmt.Sprintf("%d", hostPort)), nil
}

// Stop kills and deletes the service container (best-effort, idempotent) and
// detaches it from the space bridge if it was attached.
func (s *Space) Stop(name string) {
	s.mu.Lock()
	pid, attached := s.pids[name]
	if attached {
		delete(s.pids, name)
	}
	delete(s.ips, name)
	cnet := s.cnet
	s.mu.Unlock()
	if attached && cnet != nil {
		cnet.detach(pid)
	}
	s.ensureRuncEnter(name)
	killContainerWithin(s.runcBin, s.runcRoot, s.containerID(name), s.stopGrace)
	if s.childUserns {
		clearChildCgroup(s.containerID(name))
	}
	s.releaseOverlayHolder(cnet, name)
}

// ensureRuncEnter gives this process authority over a child-userns container
// it did not start: after an agent restart nothing has registered the space
// root yet, so it enters the container's own user namespace as its root,
// read from the pid file runc left in the bundle. Every container of a space
// runs as that one root, so any live one is enough.
func (s *Space) ensureRuncEnter(name string) {
	if !s.childUserns || runcEnterFor(s.runcRoot) != nil {
		return
	}
	raw, err := os.ReadFile(filepath.Join(childRunDir(s.bundleDir(name)), "init.pid"))
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return
	}
	if _, err := os.Stat(fmt.Sprintf("/proc/%d/ns/user", pid)); err != nil {
		return
	}
	setRuncEnter(s.runcRoot, ociproc.EnterArgs(pid, false))
}

// SetStopGrace bounds how long Stop waits after SIGTERM before SIGKILL.
func (s *Space) SetStopGrace(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d > 0 {
		s.stopGrace = d
	}
}

// Close tears the space network down (worker netns destroyed → bridge and
// every veth gone, egress slirp killed). Idempotent; a no-op for a standalone
// space. The agent calls it after every service container is stopped.
func (s *Space) Close() {
	s.mu.Lock()
	cnet := s.cnet
	ing := s.ingress
	s.cnet = nil
	s.ingress = nil
	s.gwPorts = map[string]int{} // a re-opened space re-allocates fresh ports
	s.mu.Unlock()
	// Stop accepting host ingress BEFORE the space netns is destroyed, so no
	// new connection is handed to a worker whose backend is about to vanish.
	if ing != nil {
		ing.Close()
	}
	// Backstop against leaks: force-delete EVERY deploy container still in the
	// runc state, not only the ids this process tracked. A readiness failure,
	// an attach error, or a death mid-teardown can leave a container `running`;
	// the agent's per-id Stop misses any it never recorded. Scoped to this
	// space's prefix so a shared state root never reaps another space's ids.
	reapAllContainers(s.runcBin, s.runcRoot, s.scope.containerIDPrefix())
	// Second backstop, one layer down: runc state can be gone (deleted, or never
	// written because the run died between spawn and state flush) while the
	// service processes and their cgroups survive. Then the reap above sees an
	// empty list and the next run hits "container's cgroup is not empty" with no
	// sklp-native remedy. The cgroup is the last handle, so drain it directly —
	// scoped to this space's id prefix, which cannot match another space.
	reapSpaceCgroups(s.scope.containerIDPrefix())
	if cnet != nil {
		cnet.close()
	}
}

// serviceNameOf strips the agent's "-<i>" / "-v<gen>-<i>" instance suffix from
// an instance id, yielding the service name registered in the space DNS so all
// replicas of a service share one name (spacenet round-robins across the IPs).
// The trailing segment must be a number (the replica index) to be stripped, so
// a service literally named "web-api" is left intact.
func serviceNameOf(instanceID string) string {
	head, idx, ok := splitTrailingNumber(instanceID)
	if !ok {
		return instanceID
	}
	_ = idx
	// A rollout id is "<svc>-v<gen>-<i>" (gen >= 1, folded in to avoid id
	// collisions across successive rollouts — see agent.go's rolloutGen): after
	// dropping "-<i>", strip a trailing "-v<gen>" (gen is glued to "v", no
	// separator, so this is NOT another splitTrailingNumber — "-v1" is not
	// "-<number>").
	if svc, ok := stripTrailingGenMarker(head); ok {
		return svc
	}
	// Back-compat: a bare "-v-<i>" (no generation number) is the pre-fix shape,
	// still possible for an instance already running under it.
	if len(head) >= 2 && head[len(head)-2:] == "-v" {
		return head[:len(head)-2]
	}
	return head
}

// stripTrailingGenMarker strips a trailing "-v<digits>" marker (e.g. "-v12")
// from s, returning the head and true. ok is false when s has no such suffix.
func stripTrailingGenMarker(s string) (head string, ok bool) {
	i := len(s) - 1
	for i >= 0 && s[i] >= '0' && s[i] <= '9' {
		i--
	}
	if i == len(s)-1 || i < 1 || s[i] != 'v' || s[i-1] != '-' {
		return "", false
	}
	return s[:i-1], true
}

// splitTrailingNumber splits "<head>-<n>" where <n> is all digits, returning
// the head and the number string. ok is false when there is no such suffix.
func splitTrailingNumber(s string) (head, num string, ok bool) {
	i := len(s) - 1
	for i >= 0 && s[i] >= '0' && s[i] <= '9' {
		i--
	}
	if i == len(s)-1 || i < 0 || s[i] != '-' {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}

// CacheRoot is the space's host cache root (where rootfs/, blobs/, bundles live).
// Exposed so the dev backend can build a runner rootfs under the SAME root the
// launcher reads from, keeping one cache per host.
func (s *Space) CacheRoot() string { return s.cacheRoot }

// bundleDir is the per-service bundle directory, derived deterministically from
// the service name so Start, ProbeCoords, and LogPath all agree.
func (s *Space) bundleDir(name string) string {
	return filepath.Join(s.scope.bundleRoot(), "svc-"+sanitize(name))
}

// LogPath is the host-side file capturing a service's container stdout/stderr
// (the same path Start passes to startDetached). The deploy agent tails it to
// stream container logs to the CLI.
func (s *Space) LogPath(name string) string {
	return filepath.Join(s.bundleDir(name), "service.log")
}

// Running reports whether the service container is still in the runc "running"
// (or "created") state. `sklp dev` polls it to detect a crashed service WITHOUT
// reconciling it (dev surfaces the crash; deploy is the one that restarts).
// A missing/unreadable state reads as not-running (the container is gone).
func (s *Space) Running(name string) bool {
	switch containerStatus(s.runcBin, s.runcRoot, s.containerID(name)) {
	case "running", "created":
		return true
	default:
		return false
	}
}

// ExecCoords returns the runc binary, state root, and container id for a managed
// instance, derived deterministically from its name (same id Start uses). It is
// the coordinate set `sklp deploy exec` needs to `runc exec` into a live
// instance. Pure (no FS side effect); the caller checks the container is actually
// running before exec'ing.
func (s *Space) ExecCoords(name string) (runcBin, runcRoot, containerID string) {
	return s.runcBin, s.runcRoot, s.containerID(name)
}

// ProbeCoords returns the readiness-probe coordinates for a service, derived
// deterministically from its name (same id and bundle log path Start uses), so
// the agent can gate on WaitServiceReady after launch.
func (s *Space) ProbeCoords(name string) ProbeCoords {
	c := ProbeCoords{
		RuncBin:  s.runcBin,
		RuncRoot: s.runcRoot,
		ID:       s.containerID(name),
		LogPath:  s.LogPath(name),
	}
	// In space mode, prefer the reliable native dial from the bridge netns to
	// the container's IP over `runc exec nc/wget` (which needs those binaries in
	// the image). Falls back to the runc-exec path when the IP is not yet known
	// or the space is standalone.
	s.mu.Lock()
	cnet := s.cnet
	ip := s.ips[name]
	c.Port = s.ports[name]
	s.mu.Unlock()
	if cnet != nil && ip != "" {
		c.SpaceProbe = cnet
		c.ContainerIP = ip
	}
	return c
}

// prepareChildBundle makes a bundle writable by the host again before it is
// rewritten for a child-userns service: the directory stays the operator's,
// traversable (0711) so the child's root reaches its files, and whatever the
// previous start handed to the child is removed so the rewrite can create it.
func (s *Space) prepareChildBundle(cnet *spaceNet, bundle string) error {
	if st, err := os.Stat(bundle); err == nil {
		if sys, ok := st.Sys().(*syscall.Stat_t); ok && int(sys.Uid) != os.Getuid() {
			if err := cnet.reclaimFromChild(bundle); err != nil {
				return err
			}
		}
	}
	if err := s.openPathsToChild(); err != nil {
		return err
	}
	for _, d := range []string{s.scope.bundleRoot(), bundle} {
		if err := os.Chmod(d, 0o711); err != nil {
			return err
		}
	}
	if err := os.Remove(filepath.Join(bundle, "config.json")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// openPathsToChild opens, by name only, the cache root and the directory of
// this binary to the space's root, which runs as a subuid.
func (s *Space) openPathsToChild() error {
	if err := openPathToOthers(s.cacheRoot); err != nil {
		return err
	}
	if s.selfExe != "" {
		if err := openPathToOthers(filepath.Dir(s.selfExe)); err != nil {
			return err
		}
	}
	return nil
}

// openPathToOthers lets any uid pass through dir and every ancestor the
// operator owns, by name only (+x, never +r). The child root, a subuid, must
// reach runc, the bundles and its rootfs copies under the cache root, and a
// login's $HOME (0750 on Ubuntu 24.04) or ~/.cache (0700 by convention) stops
// it dead: runc exits 126 and nsenter cannot open the holder's namespaces.
func openPathToOthers(dir string) error {
	me := os.Getuid()
	for d := filepath.Clean(dir); d != "/" && d != "."; d = filepath.Dir(d) {
		st, err := os.Stat(d)
		if err != nil {
			return err
		}
		sys, ok := st.Sys().(*syscall.Stat_t)
		if !ok || int(sys.Uid) != me {
			return nil
		}
		want := st.Mode().Perm() | 0o011
		if want == st.Mode().Perm() {
			continue
		}
		if err := os.Chmod(d, want); err != nil {
			return err
		}
	}
	return nil
}

// childRunDir is the part of a bundle that belongs to the child's root: runc
// writes its pid file there through a temp file, which takes the directory.
func childRunDir(bundle string) string { return filepath.Join(bundle, "run") }

// handChildBundleFiles gives runc, running as the child's root, what it
// touches in the bundle: config.json to read, the run dir to write. The host
// keeps the bundle directory itself.
func (s *Space) handChildBundleFiles(cnet *spaceNet, bundle string) error {
	if err := os.MkdirAll(childRunDir(bundle), 0o755); err != nil {
		return err
	}
	return cnet.handToChild(s.childRootID, filepath.Join(bundle, "config.json"), childRunDir(bundle))
}

// handNewVolumes gives a volume that was just born, an empty directory the
// operator created, to the id the service writes it as. A volume with data in
// it is never touched here: handing data over is a migration, explicit and
// once (sklet space migrate-owner), and a start that did it on its own is how
// #1260 rechowned production volumes at every launch.
func (s *Space) handNewVolumes(cnet *spaceNet, ident ociproc.Identity, svcName string, mounts []DevMount, containerUID int, imageTree string) {
	wantHost, ok := ident.HostUIDOf(containerUID)
	if !ok {
		return
	}
	hand, foreign := volumesToHand(mounts, s.volumesRoot(), wantHost)
	takeImageModes(hand, mounts, imageTree)
	child := s.childUserns && cnet != nil
	if child {
		if len(hand)+len(foreign) == 0 && !anyUnder(mounts, s.volumesRoot()) {
			return
		}
		if err := s.openVolumePathToChild(cnet); err != nil {
			fmt.Fprintf(os.Stderr, "[sklp deploy] service %q: open the volume path to the child root: %v\n", svcName, err)
		}
	}
	if len(hand) > 0 {
		var err error
		if child {
			owner := s.childRootID
			if containerUID > 0 {
				owner = containerUID
			}
			err = cnet.handToChild(owner, hand...)
		} else {
			err = lchownAll(hand, wantHost)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "[sklp deploy] service %q: hand new volumes to uid %d: %v\n", svcName, wantHost, err)
		}
	}
	for _, f := range foreign {
		fmt.Fprintf(os.Stderr, "[sklp deploy] service %q: volume %s belongs to another uid than %d; it holds data, so it stays as it is (sklet space migrate-owner hands it over)\n", svcName, f, wantHost)
	}
}

// takeImageModes gives a newborn volume the mode the image has at its mount
// point, as Docker does when it creates a named volume: postgres:18 ships
// /var/lib/postgresql 1777 because its entrypoint drops to the postgres user
// before creating PGDATA inside it, and a 0700 volume there refuses it.
func takeImageModes(newborn []string, mounts []DevMount, imageTree string) {
	if imageTree == "" {
		return
	}
	born := make(map[string]bool, len(newborn))
	for _, p := range newborn {
		born[p] = true
	}
	for _, m := range mounts {
		if !born[m.Source] {
			continue
		}
		st, err := os.Stat(filepath.Join(imageTree, filepath.Clean("/"+m.Destination)))
		if err != nil || !st.IsDir() {
			continue
		}
		_ = os.Chmod(m.Source, st.Mode()&(fs.ModePerm|fs.ModeSticky|fs.ModeSetgid))
	}
}

func lchownAll(paths []string, uid int) error {
	for _, p := range paths {
		if err := os.Lchown(p, uid, uid); err != nil {
			return err
		}
	}
	return nil
}

// openVolumePathToChild lets the child root reach the volumes root, which
// runc must open to bind-mount a volume: the two per-space directories take
// the child root's group and 0710, the shared ones above them 0711, so any
// uid may pass through by name and only the space's root enters the
// space's own.
func (s *Space) openVolumePathToChild(cnet *spaceNet) error {
	volumes := s.volumesRoot()
	spaceDir := filepath.Dir(volumes)
	spacesDir := filepath.Dir(spaceDir)
	for _, d := range []string{filepath.Dir(spacesDir), spacesDir} {
		if err := os.Chmod(d, 0o711); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	for _, d := range []string{spaceDir, volumes} {
		if err := os.Chmod(d, 0o710); err != nil {
			return err
		}
	}
	return cnet.runAsSpaceRoot("chgrp", strconv.Itoa(s.childRootID), spaceDir, volumes)
}

func anyUnder(mounts []DevMount, root string) bool {
	for _, m := range mounts {
		if strings.HasPrefix(m.Source, root+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// volumesToHand sorts the volume mounts under volumesRoot: empty directories
// not yet owned by wantUID are handed over; non-empty ones with another owner
// are reported.
func volumesToHand(mounts []DevMount, volumesRoot string, wantUID int) (hand, foreign []string) {
	prefix := volumesRoot + string(os.PathSeparator)
	for _, m := range mounts {
		if !strings.HasPrefix(m.Source, prefix) {
			continue
		}
		st, err := os.Stat(m.Source)
		if err != nil {
			continue
		}
		if sys, ok := st.Sys().(*syscall.Stat_t); ok && int(sys.Uid) == wantUID {
			continue
		}
		entries, err := os.ReadDir(m.Source)
		if err != nil {
			continue
		}
		if len(entries) == 0 {
			hand = append(hand, m.Source)
		} else {
			foreign = append(foreign, m.Source)
		}
	}
	return hand, foreign
}
