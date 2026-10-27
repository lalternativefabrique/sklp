package space

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/lalternativefabrique/sklp/space/ociproc"
)

// writeRunnerBundle writes the runner's config.json. join == nil makes the
// runner the leader (creates its own namespaces — the single-container
// case); a non-nil join makes it join the leader service's shared netns +
// userns by path.
func writeRunnerBundle(bundle, rootfs, cgroupsPath string, cfg Config, join *joinNamespaces) error {
	uidMappings, gidMappings, err := ociproc.ResolveIDMappings(os.Getuid(), os.Getgid())
	if err != nil {
		return err
	}
	spec := buildRunnerSpecWithMappings(rootfs, cgroupsPath, cfg, uidMappings, gidMappings, join)
	b, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return err
	}
	// 0600: config.json embeds secrets as process.env; never world-readable.
	return os.WriteFile(filepath.Join(bundle, "config.json"), b, 0o600)
}

// createRuntimeHook describes the paired OCI hooks a space service spec
// carries. createRuntime moves the veth from the worker; createContainer runs
// inside the container netns and configures that peer. nil means no hooks.
type createRuntimeHook struct {
	Path         string
	MoveEnv      []string
	ConfigureEnv []string
}

// writeServiceBundleNet writes a sidecar service's config.json, with optional
// resolvConf and hosts paths bind-mounted at /etc/resolv.conf and /etc/hosts
// (the space path passes the space resolver's + generated hosts files), and an
// optional createRuntime hook (space path attaches the network before exec; nil
// disables it). join == nil marks the leader; a non-nil join reuses the shared
// namespaces.
func writeServiceBundleNet(bundle, rootfs string, svc Service, imgEntrypoint, imgCmd, imgEnv []string, workdir string, join *joinNamespaces, resolvConf, hosts string, hook *createRuntimeHook, overlayRoot bool) error {
	uidMappings, gidMappings, err := ociproc.ResolveIDMappings(os.Getuid(), os.Getgid())
	if err != nil {
		return err
	}
	spec := buildServiceSpecWithMappings(rootfs, svc, imgEntrypoint, imgCmd, imgEnv, workdir, uidMappings, gidMappings, join)
	if overlayRoot {
		applyOverlayRoot(spec)
	}
	if resolvConf != "" {
		addResolvConfMount(spec, resolvConf)
	}
	if hosts != "" {
		addHostsMount(spec, hosts)
	}
	if hook != nil {
		addCreateRuntimeHook(spec, hook.Path, hook.MoveEnv, hook.ConfigureEnv)
	}
	b, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return err
	}
	// 0600: config.json embeds secrets as process.env; never world-readable.
	return os.WriteFile(filepath.Join(bundle, "config.json"), b, 0o600)
}

// joinNamespaces, when non-nil, makes a spec JOIN the leader's network
// and user namespaces by path instead of creating fresh ones. The kernel
// refuses setns(NS_NET) into a netns owned by a different user ns, so we
// always join BOTH and drop uid/gid mappings (remapping a joined user ns
// is rejected). nil = leader behaviour (create fresh namespaces), which
// is byte-identical to the previous single-container spec.
//
// PrivateNetns flips the network half: the container still joins the
// shared USER namespace by UsernsPath, but keeps its OWN (empty) network
// namespace instead of joining NetnsPath. This is the space-deploy mode
// (spacenet): every service shares the space userns — required for spacenet
// to push a veth into the service netns — yet each service has a private
// netns the space bridge attaches after launch. NetnsPath is ignored
// when PrivateNetns is set.
type joinNamespaces struct {
	// NetnsPath is /proc/<leaderInitPID>/ns/net. Ignored when PrivateNetns.
	NetnsPath string
	// UsernsPath is /proc/<leaderInitPID>/ns/user (or the space worker's).
	UsernsPath string
	// PrivateNetns keeps a fresh network ns while still joining UsernsPath.
	PrivateNetns bool
}

// applyJoin rewrites linux.namespaces so the container joins the leader's
// netns + userns by path, and removes the uid/gid mappings the kernel
// rejects on a joined user ns. It is a no-op when join is nil.
//
// With join.PrivateNetns it joins ONLY the user ns by path and declares a
// fresh network ns (no path) — the space-deploy variant. The uid/gid
// mappings are still dropped: a joined user ns cannot be remapped, exactly
// as in the shared-netns case.
func applyJoin(linux map[string]any, join *joinNamespaces) {
	if join == nil {
		return
	}
	netns := map[string]any{"type": "network", "path": join.NetnsPath}
	if join.PrivateNetns {
		netns = map[string]any{"type": "network"} // fresh, spacenet attaches later
	}
	linux["namespaces"] = []map[string]any{
		{"type": "pid"},
		{"type": "ipc"},
		{"type": "uts"},
		{"type": "mount"},
		{"type": "user", "path": join.UsernsPath},
		netns,
	}
	delete(linux, "uidMappings")
	delete(linux, "gidMappings")
}

// applyOverlayRoot rewrites a service spec for the writable-overlay-rootfs path:
// runc is `nsenter`d into a holder's user + mount namespaces (where the overlay
// merged is mounted), so the spec must NOT declare or join a user namespace
// itself (runc already runs in the entered, mapped userns — a second userns or
// uidMappings would double-map and fail), and the root is writable (the merged
// overlay; image immutability is enforced by the read-only lower, not the flag).
// The netns join (PrivateNetns path) stays — the holder kept the worker netns,
// and the post-start veth attach is unchanged.
//
// It also DROPS the fixed /tmp, /run, /var/run tmpfs mounts. On the read-only
// path those are the only writable spots, but over a writable overlay they are
// actively harmful: an empty tmpfs MASKS whatever the image ships under that path
// (e.g. mariadb's /run/mysqld, which its entrypoint `find`s and dies without).
// The overlay already makes every path writable via copy-up while preserving the
// image's content, so the tmpfs are both redundant and a masking bug. /proc,
// /dev/pts, and the bind mounts (resolv.conf, hosts, volumes) are kept.
func applyOverlayRoot(spec map[string]any) {
	if root, ok := spec["root"].(map[string]any); ok {
		root["readonly"] = false
	}
	if mounts, ok := spec["mounts"].([]map[string]any); ok {
		masking := map[string]bool{"/tmp": true, "/run": true, "/var/run": true}
		kept := mounts[:0]
		for _, m := range mounts {
			dst, _ := m["destination"].(string)
			if m["type"] == "tmpfs" && masking[dst] {
				continue // overlay makes it writable AND keeps the image's content
			}
			kept = append(kept, m)
		}
		spec["mounts"] = kept
	}
	linux, ok := spec["linux"].(map[string]any)
	if !ok {
		return
	}
	delete(linux, "uidMappings")
	delete(linux, "gidMappings")
	// Drop the user namespace entry: runc inherits the holder's via nsenter.
	if nss, ok := linux["namespaces"].([]map[string]any); ok {
		kept := nss[:0]
		for _, ns := range nss {
			if ns["type"] == "user" {
				continue
			}
			kept = append(kept, ns)
		}
		linux["namespaces"] = kept
	}
}

// buildSpec assembles the OCI runtime-spec as a serialisable map. It is
// kept separate from the bundle writer so the spec can be asserted in tests
// without touching the filesystem or the host uid/gid.
//
// buildSpec is a pure mapping: it adds linux.resources iff cfg requests a
// limit, and linux.cgroupsPath iff cgroupsPath is non-empty. The decision
// of whether limits can actually be honoured (cgroup delegation) lives in
// Run, which zeroes the limits and passes an empty cgroupsPath when not.
func buildSpec(rootfs, cgroupsPath string, cfg Config, uid, gid int) map[string]any {
	uidMappings, gidMappings := ociproc.IDMappings(uid, gid)
	return buildSpecWithMappings(rootfs, cgroupsPath, cfg, uidMappings, gidMappings)
}

func buildSpecWithMappings(rootfs, cgroupsPath string, cfg Config, uidMappings, gidMappings []map[string]any) map[string]any {
	taskName := cfg.TaskName
	if taskName == "" {
		taskName = "ci"
	}
	args := append([]string{
		"sklp", "run", taskName,
		"-repo", "/workspace",
		"-output", "json",
	}, cfg.ExtraArgs...)

	env := append([]string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/tmp",
	}, cfg.Env...)

	linux := map[string]any{
		"namespaces": []map[string]any{
			{"type": "pid"},
			{"type": "ipc"},
			{"type": "uts"},
			{"type": "mount"},
			{"type": "user"},
			{"type": "network"},
			{"type": "cgroup"},
		},
		"uidMappings":   uidMappings,
		"gidMappings":   gidMappings,
		"maskedPaths":   ociproc.DefaultMaskedPaths(),
		"readonlyPaths": ociproc.DefaultReadonlyPaths(),
		"seccomp":       ociproc.DefaultSeccomp(),
	}
	// Only emit cgroup keys when limits are requested, so a no-limit run
	// produces a spec byte-identical to the pre-cgroups behaviour.
	if res, ok := resourcesSpec(cfg); ok {
		linux["resources"] = res
	}
	if cgroupsPath != "" {
		linux["cgroupsPath"] = cgroupsPath
	}

	mounts := []map[string]any{
		{"destination": "/proc", "type": "proc", "source": "proc"},
		{"destination": "/sys/fs/cgroup", "type": "cgroup", "source": "cgroup",
			"options": []string{"nosuid", "noexec", "nodev", "relatime"}},
		{"destination": "/workspace", "type": "bind", "source": cfg.RepoPath,
			"options": []string{"rbind", "rw"}},
		{"destination": "/tmp", "type": "tmpfs", "source": "tmpfs",
			"options": []string{"nosuid", "nodev", "mode=1777", "size=2g"}},
	}
	// When /workspace is a linked git worktree, its `.git` is a file whose
	// `gitdir:` points at the main repo's .git/worktrees/<name>/ by absolute
	// host path — outside /workspace, so the rbind above does not bring it in.
	// Bind-mount the main repo at that same absolute path so git resolves the
	// pointer inside the container (else `git config`/`go mod` see no repo and
	// the token wiring + private fetch break). ro: the run never writes there.
	if main := mainRepoForWorktree(cfg.RepoPath); main != "" {
		mounts = append(mounts, map[string]any{
			"destination": main, "type": "bind", "source": main,
			"options": []string{"rbind", "ro"},
		})
	}

	return map[string]any{
		"ociVersion": "1.0.2",
		"process": map[string]any{
			"terminal": false,
			"user":     map[string]any{"uid": 0, "gid": 0},
			"args":     args,
			"env":      env,
			"cwd":      "/workspace",
			"capabilities": map[string]any{
				"bounding":    defaultCaps(),
				"effective":   defaultCaps(),
				"permitted":   defaultCaps(),
				"inheritable": defaultCaps(),
				"ambient":     defaultCaps(),
			},
			"noNewPrivileges": true,
		},
		"root": map[string]any{
			"path":     rootfs,
			"readonly": false,
		},
		"hostname": "sklp",
		"mounts":   mounts,
		"linux":    linux,
	}
}

// buildRunnerSpec is buildSpec plus an optional netns/userns join. When
// join is nil it returns the exact single-container spec (the runner is
// the leader and creates its own namespaces). When join is non-nil the
// runner joins the leader service's shared namespaces by path.
func buildRunnerSpec(rootfs, cgroupsPath string, cfg Config, uid, gid int, join *joinNamespaces) map[string]any {
	spec := buildSpec(rootfs, cgroupsPath, cfg, uid, gid)
	applyJoin(spec["linux"].(map[string]any), join)
	return spec
}

func buildRunnerSpecWithMappings(rootfs, cgroupsPath string, cfg Config, uidMappings, gidMappings []map[string]any, join *joinNamespaces) map[string]any {
	spec := buildSpecWithMappings(rootfs, cgroupsPath, cfg, uidMappings, gidMappings)
	applyJoin(spec["linux"].(map[string]any), join)
	return spec
}

// buildServiceSpec assembles the OCI runtime-spec for a sidecar service
// container. The leader (join == nil) creates the shared netns + userns
// with the full subuid/subgid mapping (needed for images like postgres
// that chown their data dir to a non-root uid). Joiners reuse the
// leader's namespaces by path.
//
// Unlike the runner spec, a service has no /workspace bind mount and no
// inner `sklp run` argv: it runs the image's own entrypoint+cmd
// (imgEntrypoint+imgCmd from the image config), optionally overridden by
// svc.Command. imgEnv layers under svc.Env so the service env overlay
// wins on key collision (OCI keeps the last duplicate).
func buildServiceSpec(rootfs string, svc Service, imgEntrypoint, imgCmd, imgEnv []string, workdir string, uid, gid int, join *joinNamespaces) map[string]any {
	uidMappings, gidMappings := ociproc.IDMappings(uid, gid)
	return buildServiceSpecWithMappings(rootfs, svc, imgEntrypoint, imgCmd, imgEnv, workdir, uidMappings, gidMappings, join)
}

func buildServiceSpecWithMappings(rootfs string, svc Service, imgEntrypoint, imgCmd, imgEnv []string, workdir string, svcUIDMappings, svcGIDMappings []map[string]any, join *joinNamespaces) map[string]any {
	// The image's PATH is authoritative (docker semantics): only inject a
	// fallback PATH when the image config carries none, so we never shadow a
	// PATH like postgres's /usr/lib/postgresql/17/bin with a generic default.
	var env []string
	if !hasEnvKey(imgEnv, "PATH") {
		env = append(env, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	env = append(env, imgEnv...)
	env = append(env, svc.Env...)

	// Namespaces. We keep the USER namespace (and its uid/gid mappings) even
	// with host networking: filesystem + uid isolation is independent of the
	// netns, and dropping it would run traefik as the real host root. Binding
	// :443/:80 from inside the userns still works — CAP_NET_BIND_SERVICE is in
	// the bounding set (defaultCaps) and the bind happens in the HOST netns,
	// where the mapped container-root holds that cap. Only the network
	// namespace is conditional.
	namespaces := []map[string]any{
		{"type": "pid"},
		{"type": "ipc"},
		{"type": "uts"},
		{"type": "mount"},
		{"type": "user"},
	}
	if !svc.HostNetwork {
		// Default: a fresh private netns. HostNetwork omits this entry so the
		// container shares the host netns and binds host ports directly.
		namespaces = append(namespaces, map[string]any{"type": "network"})
	}
	if svc.NestedRuntime {
		namespaces = append(namespaces, map[string]any{"type": "cgroup"})
	}
	maskedPaths, readonlyPaths, seccomp := ociproc.DefaultMaskedPaths(), ociproc.DefaultReadonlyPaths(), ociproc.DefaultSeccomp()
	if svc.NestedRuntime {
		maskedPaths, readonlyPaths, seccomp = ociproc.NestableMaskedPaths(), []string{}, ociproc.NestableSeccomp()
	}
	linux := map[string]any{
		"namespaces":    namespaces,
		"uidMappings":   svcUIDMappings,
		"gidMappings":   svcGIDMappings,
		"maskedPaths":   maskedPaths,
		"readonlyPaths": readonlyPaths,
		"seccomp":       seccomp,
	}
	if svc.NestedRuntime {
		linux["devices"] = []map[string]any{{
			"path": "/dev/net/tun", "type": "c", "major": 10, "minor": 200, "fileMode": 0o666,
		}}
	}
	if res, ok := ociproc.ResourcesSpec(svc.Resources); ok {
		linux["resources"] = res
		if svc.CgroupsPath != "" {
			linux["cgroupsPath"] = svc.CgroupsPath
		}
	}
	applyJoin(linux, join)

	cwd := "/"
	if workdir != "" {
		cwd = workdir
	}
	args := serviceArgs(svc.Command, imgEntrypoint, imgCmd, svc.IgnoreEntrypoint)
	// PauseInit overrides Command: park the init on sklp's own pause binary so
	// keep-alive never depends on the guest rootfs shipping `sleep`/`sh`. If the
	// rootfs already bakes sklp (the CI runner does, at /usr/local/bin/sklp) run
	// it in place; otherwise bind the running sklp (/proc/self/exe, static) in
	// read-only at /.sklp/pause. pauseMount is nil in the baked case.
	var pauseMount map[string]any
	if svc.PauseInit {
		pauseBin, mnt := resolvePauseInit(rootfs)
		args, pauseMount = []string{pauseBin, "pause"}, mnt
	}
	// A deployed service gets the narrow service baseline, not the CI runner's
	// set, plus whatever its manifest asked for explicitly.
	caps := append(ociproc.ServiceCapabilities(), svc.Capabilities...)
	if svc.HostNetwork {
		caps = append(caps, "CAP_NET_BIND_SERVICE")
	} else {
		linux["sysctl"] = map[string]string{"net.ipv4.ip_unprivileged_port_start": "0"}
	}
	if svc.NestedRuntime {
		caps = append(caps, "CAP_SYS_ADMIN")
	}
	process := map[string]any{
		"terminal": false,
		"user":     map[string]any{"uid": svc.ImageUID, "gid": svc.ImageGID},
		"args":     args,
		"env":      env,
		"cwd":      cwd,
		"capabilities": map[string]any{
			"bounding":    caps,
			"effective":   caps,
			"permitted":   caps,
			"inheritable": caps,
			"ambient":     caps,
		},
		"noNewPrivileges": true,
	}

	// The rootfs is mounted read-only (see "root" below), so every path an
	// image legitimately writes at runtime is redirected to a tmpfs: /tmp for
	// caches + HOME, and /run + /var/run for sockets/pid files (postgres opens
	// its unix socket under /var/run/postgresql, nats writes /run). Without
	// these the read-only rootfs aborts those images at boot. Persistent data
	// (postgres PGDATA, nats store) rides its declared volume, not the rootfs.
	mounts := []map[string]any{
		{"destination": "/proc", "type": "proc", "source": "proc"},
		{"destination": "/tmp", "type": "tmpfs", "source": "tmpfs",
			"options": []string{"nosuid", "nodev", "mode=1777", "size=2g"}},
		{"destination": "/run", "type": "tmpfs", "source": "tmpfs",
			"options": []string{"nosuid", "nodev", "mode=0755"}},
		{"destination": "/var/run", "type": "tmpfs", "source": "tmpfs",
			"options": []string{"nosuid", "nodev", "mode=0755"}},
		// POSIX shared memory. OpenMP/oneTBB runtimes shm_open() here during
		// library init — before main(), so no env var can steer them — and
		// abort with "Can't open SHM2" when the path is absent. Anything built
		// on them (TEI, PyTorch dataloaders, Chrome) needs it to boot at all.
		{"destination": "/dev/shm", "type": "tmpfs", "source": "shm",
			"options": []string{"nosuid", "nodev", "noexec", "mode=1777", "size=1g"}},
		devptsMount(),
	}
	// /sys omitted for HostNetwork: user namespace without CAP_SYS_ADMIN cannot
	// mount a new sysfs. HostNetwork services (traefik) do not need sysfs.
	if !svc.HostNetwork {
		mounts = append([]map[string]any{
			mounts[0],
			{"destination": "/sys", "type": "sysfs", "source": "sysfs",
				"options": []string{"nosuid", "noexec", "nodev", "ro"}},
		}, mounts[1:]...)
	}
	// runc inside the service refuses to start without a cgroup mount in
	// mountinfo. An application image must not see one: timescaledb-tune
	// sizes itself from it and panics on the hierarchy a rootless runc shows.
	if svc.NestedRuntime {
		mounts = append(mounts, map[string]any{
			"destination": "/sys/fs/cgroup", "type": "cgroup", "source": "cgroup",
			"options": []string{"nosuid", "noexec", "nodev", "relatime"},
		})
	}
	// OPT-IN dev bind-mounts. svc.DevMounts is nil on the deploy/CI path, so
	// the slice above stays byte-identical to today's spec there. Only the
	// `sklp dev` path fills it (repo hot-reload). The mount writes as the
	// host uid (mapped to container uid 0 by uidMappings), so repo files the
	// container creates stay operator-owned, never root.
	for _, dm := range svc.DevMounts {
		mounts = append(mounts, devBindMount(dm))
	}
	// The pause binary bind (nil when the rootfs already bakes sklp).
	if pauseMount != nil {
		mounts = append(mounts, pauseMount)
	}

	return map[string]any{
		"ociVersion": "1.0.2",
		"process":    process,
		// Immutable rootfs: a service can never mutate its own image, so it
		// cannot pollute the digest-shared rootfs other services on the same
		// image reuse, and a run leaves no trace in the image cache. This is
		// the isolation + dev↔CI parity guarantee — writes go to the tmpfs /
		// volume / bind mounts above, never the image.
		"root": map[string]any{
			"path":     rootfs,
			"readonly": true,
		},
		"hostname": "sklp",
		"mounts":   mounts,
		"linux":    linux,
	}
}

// hasEnvKey reports whether env (KEY=VALUE entries) defines key.
func hasEnvKey(env []string, key string) bool {
	pfx := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, pfx) {
			return true
		}
	}
	return false
}

// devBindMount renders one DevMount as an OCI bind mount. rw by default for
// hot-reload; ro when DevMount.ReadOnly. rbind so submounts under the source
// (e.g. a nested worktree) come along, matching the runner /workspace mount.
func devBindMount(dm DevMount) map[string]any {
	access := "rw"
	if dm.ReadOnly {
		access = "ro"
	}
	return map[string]any{
		"destination": dm.Destination,
		"type":        "bind",
		"source":      dm.Source,
		"options":     []string{"rbind", access},
	}
}

// pauseGuestPath is where the bind-mounted pause binary lands inside a guest
// whose rootfs does NOT bake sklp. The reaper matches on this too.
const pauseGuestPath = "/.sklp/pause"

// resolvePauseInit picks how `sklp pause` reaches the container init. If the
// rootfs already bakes sklp at /usr/local/bin/sklp (the CI runner does), run it
// in place — no mount. Otherwise the guest rootfs may ship neither sklp nor
// even `sleep`/`sh` (a minimal space.yaml rootfs), so bind the running sklp
// binary read-only at pauseGuestPath. Returns the in-container arg0 and the
// mount to add (nil in the baked case).
//
// The source is the running sklp's on-disk path, resolved via /proc/self/exe.
// We must NOT bind /proc/self/exe itself: it is a magic symlink and the kernel
// rejects MS_BIND on it with EINVAL. Readlink yields the real regular file
// (e.g. ~/.local/bin/sklp), which binds fine. If resolution fails we fall back
// to no mount + `sklp` on PATH (best effort; the baked case already returned).
func resolvePauseInit(rootfs string) (string, map[string]any) {
	if baked := filepath.Join(rootfs, "usr/local/bin/sklp"); fileExists(baked) {
		return "/usr/local/bin/sklp", nil
	}
	self, err := os.Readlink("/proc/self/exe")
	if err != nil || self == "" {
		return "sklp", nil // last resort: hope PATH has it
	}
	mnt := map[string]any{
		"destination": pauseGuestPath,
		"type":        "bind",
		"source":      self,
		"options":     []string{"bind", "ro", "nosuid", "nodev"},
	}
	return pauseGuestPath, mnt
}

// fileExists reports whether path is an existing (non-dir-checked) file.
func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// devptsMount is a private (newinstance) devpts mount at /dev/pts. Without it a
// service container has no /dev/pts/ptmx, so `runc exec -t` (the interactive
// `sklp deploy exec` path) fails with "open /dev/pts/ptmx: no such device" and
// the session is silently mute. ptmxmode=0666 lets the unprivileged container
// uid open the multiplexor; we omit gid=5 because the host tty group is not
// mapped into the container userns (an unmapped gid would make the mount fail).
func devptsMount() map[string]any {
	return map[string]any{
		"destination": "/dev/pts",
		"type":        "devpts",
		"source":      "devpts",
		"options":     []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620"},
	}
}

// addResolvConfMount bind-mounts host file src read-only at /etc/resolv.conf
// in the spec, so a space service resolves sibling service names (and
// forwarded external names) through the space DNS on the gateway. Appended
// after the image rootfs's own /etc/resolv.conf, the bind shadows it.
func addResolvConfMount(spec map[string]any, src string) {
	mounts := spec["mounts"].([]map[string]any)
	mounts = append(mounts, map[string]any{
		"destination": "/etc/resolv.conf",
		"type":        "bind",
		"source":      src,
		"options":     []string{"rbind", "ro"},
	})
	spec["mounts"] = mounts
}

// addCreateRuntimeHook installs paired OCI hooks. createRuntime runs in the
// runtime namespace and moves the veth peer into the container netns.
// createContainer then runs inside that netns and configures the peer. The
// split is required for rootless runc: during createRuntime the peer can be
// moved by pid, but reopening /proc/<pid>/ns/net may still be denied.
//
// Both hooks complete before the service command execs. A non-zero exit fails
// create, so a service whose network cannot be wired never runs unreachable.
func addCreateRuntimeHook(spec map[string]any, hookPath string, moveEnv, configureEnv []string) {
	spec["hooks"] = map[string]any{
		"createRuntime": []map[string]any{
			{
				"path": hookPath,
				"args": []string{hookPath},
				"env":  moveEnv,
			},
		},
		"createContainer": []map[string]any{
			{
				"path": hookPath,
				"args": []string{hookPath},
				"env":  configureEnv,
			},
		},
	}
}

// addHostsMount bind-mounts host file src read-only at /etc/hosts in the spec.
// A space netns ships no /etc/hosts, so a service that resolves localhost /
// 0.0.0.0 / loopback to announce itself (e.g. nats) aborts on boot. Appended
// after the image rootfs's own /etc/hosts, the bind shadows it. Mirrors
// addResolvConfMount.
func addHostsMount(spec map[string]any, src string) {
	mounts := spec["mounts"].([]map[string]any)
	mounts = append(mounts, map[string]any{
		"destination": "/etc/hosts",
		"type":        "bind",
		"source":      src,
		"options":     []string{"rbind", "ro"},
	})
	spec["mounts"] = mounts
}

// serviceArgs builds the container argv with Docker/OCI semantics: a declared
// `command` overrides the image CMD but KEEPS the image ENTRYPOINT, which is
// prepended. So nats (entrypoint [nats-server], cmd [...]) + command
// [-js -sd /data] runs `nats-server -js -sd /data`, not `-js` as the binary.
// With no command, entrypoint + the image's own cmd run (the image default).
func serviceArgs(command, imgEntrypoint, imgCmd []string, ignoreEntrypoint bool) []string {
	// --entrypoint: run command verbatim, image entrypoint discarded.
	if ignoreEntrypoint {
		return append([]string(nil), command...)
	}
	args := append([]string(nil), imgEntrypoint...)
	if len(command) > 0 {
		return append(args, command...)
	}
	return append(args, imgCmd...)
}

// defaultCaps is the capability set we grant inside the container.
// Sized to cover go build, pnpm install, git, basic chown for tooling
// caches. SYS_ADMIN is intentionally excluded — the embedded runtime
// must never grant it, since the user namespace remap is the only
// thing protecting the host from a misbehaving step.
func defaultCaps() []string { return ociproc.DefaultCapabilities() }

// mainRepoForWorktree returns the absolute path of the main repository a
// linked git worktree points to, or "" when repoPath is a plain checkout (or
// not a repo). A worktree has `.git` as a FILE containing
// `gitdir: <main>/.git/worktrees/<name>`; we walk up two levels to <main>.
// Mirrors runner/spawn.mainRepoForWorktree (kept local to avoid an import
// edge between runtime and spawn).
func mainRepoForWorktree(repoPath string) string {
	info, err := os.Stat(filepath.Join(repoPath, ".git"))
	if err != nil || info.IsDir() {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(repoPath, ".git"))
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(b))
	const prefix = "gitdir:"
	if !strings.HasPrefix(line, prefix) {
		return ""
	}
	gitdir := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	if !filepath.IsAbs(gitdir) {
		return ""
	}
	main := filepath.Dir(filepath.Dir(filepath.Dir(gitdir))) // .git/worktrees/<name> → main
	if _, err := os.Stat(main); err != nil {
		return ""
	}
	return main
}
