package space

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/lalternativefabrique/sklp/space/spaceapi"
)

// spaceInitService is the long-lived init every `space up` launches: it parks on
// a sleep so the space stays alive after the launching shell exits, and the user
// attaches a real shell via Exec (runc exec, fresh PTY). Mirrors `sklp space`.
const spaceInitService = "shell"

// runcRegistry is the runc-backed spaceapi.Registry: it owns the host space
// cache (the spaces/ dir, runc state roots) and answers every space operation
// transport-free, so the CLI and a future HTTP server are two thin adapters over
// it. Stateless beyond the cache root resolved once at construction.
type runcRegistry struct {
	cacheRoot string
	// listSupervised returns the names of spaces with a LIVE deploy control
	// socket (a deploy, not a bare space). Injected so package runtime stays
	// free of deploy/control (which imports deploy → runtime: an import cycle).
	// nil = no space is flagged supervised.
	listSupervised func() ([]string, error)
}

// NewRegistry constructs the runc-backed Registry. It resolves the host cache
// root once (the same <cache>/sklp/embedded-runtime the launcher reads) so List
// and friends scan the right spaces/ dir.
//
// listSupervised reports the spaces with a live deploy control socket; it is
// injected (rather than imported) because deploy/control sits above runtime in
// the dependency graph. Pass control.ListSpaces from the CLI; pass nil to skip
// the Supervised column entirely.
func NewRegistry(listSupervised func() ([]string, error)) (spaceapi.Registry, error) {
	root, err := cacheRoot()
	if err != nil {
		return nil, fmt.Errorf("space registry: cache root: %w", err)
	}
	return &runcRegistry{cacheRoot: root, listSupervised: listSupervised}, nil
}

// List returns every space under the cache (running or stopped), sorted by name.
// It scans the spaces/ dir for the per-space state dirs and renders each through
// the same container/volume read Get uses; the Supervised column is filled from
// the live deploy control sockets in one batch.
func (r *runcRegistry) List(ctx context.Context) ([]spaceapi.SpaceInfo, error) {
	names, err := r.spaceNames()
	if err != nil {
		return nil, err
	}
	supervised := r.supervisedSet()
	out := make([]spaceapi.SpaceInfo, 0, len(names))
	for _, name := range names {
		info, err := r.describe(name)
		if err != nil {
			return nil, err
		}
		info.Supervised = supervised[name]
		if isEmptyShell(info) {
			continue
		}
		out = append(out, info)
	}
	return out, nil
}

// isEmptyShell reports a space dir with nothing in it: no runc state, no
// volume, no bundle, no supervisor. The agent's first boot leaves one behind
// for its identity space, and listing it reads as a space that runs nothing.
func isEmptyShell(info spaceapi.SpaceInfo) bool {
	return info.Status == "" && len(info.Volumes) == 0 && info.Repo == "" && !info.Supervised
}

// Get returns one space's detail. ErrSpaceNotFound when its state dir is absent.
func (r *runcRegistry) Get(ctx context.Context, name string) (spaceapi.SpaceInfo, error) {
	name = sanitizeSpaceName(name)
	if !r.exists(name) {
		return spaceapi.SpaceInfo{}, spaceapi.ErrSpaceNotFound
	}
	info, err := r.describe(name)
	if err != nil {
		return spaceapi.SpaceInfo{}, err
	}
	info.Supervised = r.supervisedSet()[name]
	return info, nil
}

// Up materialises and launches a new detached space. It returns ErrSpaceExists
// when a space of that name already exists (the user must `down` it first), then
// builds the rootfs from .sklp/space.yaml and starts a long-lived init via
// `runc run -d`. It deliberately does NOT stop the space — the init survives the
// shell so the caller can attach to it via Exec.
func (r *runcRegistry) Up(ctx context.Context, name string, spec spaceapi.UpSpec) error {
	name = sanitizeSpaceName(name)
	if name == "" {
		return fmt.Errorf("space name must not be empty")
	}
	if r.exists(name) {
		return spaceapi.ErrSpaceExists
	}

	space, err := NewSpace(name)
	if err != nil {
		return err
	}
	// NOTE: no space.Close()/Stop here — the detached init must outlive this call.

	var rootfs string
	var env []string
	var workdir string
	if spec.Image != "" {
		// Bare-image space: pull the image and use it AS the rootfs — no recipe
		// build, no space.yaml. The per-service overlay makes it writable; env +
		// workdir come from the image config so the shell lands like `docker run`.
		rfs, cfg, ierr := prepareServiceImage(ctx, space.CacheRoot(), spec.Image, runtime.GOARCH)
		if ierr != nil {
			space.Close()
			return fmt.Errorf("space image %q: %w", spec.Image, ierr)
		}
		rootfs, env, workdir = rfs, cfg.Config.Env, cfg.Config.WorkingDir
	} else {
		repo := spec.Repo
		if repo == "" {
			repo = "."
		}
		if spec.BaseImage != "" {
			// Honour an explicit base override for this build only.
			prev, had := os.LookupEnv("SKLP_DEV_BASE_IMAGE")
			_ = os.Setenv("SKLP_DEV_BASE_IMAGE", spec.BaseImage)
			defer func() {
				if had {
					_ = os.Setenv("SKLP_DEV_BASE_IMAGE", prev)
				} else {
					_ = os.Unsetenv("SKLP_DEV_BASE_IMAGE")
				}
			}()
		}
		var berr error
		var unlockRootfs func()
		rootfs, env, workdir, unlockRootfs, berr = buildSpaceRootfs(ctx, space, repo, spec.File, nil)
		// A registry space outlives this call: its containers keep the rootfs
		// mounted long after Up returns, and only `space down` frees it. Releasing
		// here would let the collector reclaim a rootfs under a running space, so
		// the pin is dropped immediately and liveness comes from the space dir
		// instead (spaceHoldsRunner).
		unlockRootfs()
		if berr != nil {
			space.Close()
			return berr
		}
	}

	ds := DeployService{
		Name:             spaceInitService,
		RootfsDir:        rootfs,
		RootfsCfg:        ImageConfig{Env: env, WorkingDir: workdir},
		Env:              env,
		IgnoreEntrypoint: true,
		// Park the init on sklp pause; real work attaches via runc exec.
		PauseInit: true,
	}
	if _, err := space.Start(ctx, ds); err != nil {
		space.Close()
		return fmt.Errorf("start space %q: %w", name, err)
	}
	return nil
}

// Down stops and removes a space, its named volumes included. A missing space
// is not an error (idempotent).
func (r *runcRegistry) Down(ctx context.Context, name string) error {
	name = sanitizeSpaceName(name)
	if name == "" {
		return nil
	}
	// A missing state dir does NOT mean nothing is running: the dir is the only
	// handle sklp keeps, so once it is gone (reclaimed after a run, pruned) any
	// surviving service process is invisible to runc and to List. Drain the
	// cgroups under this space's id prefix before returning, or the leak is
	// unfixable without a manual kill and every later run fails on "container's
	// cgroup is not empty".
	if !r.exists(name) {
		reapSpaceCgroups(r.scope(name).containerIDPrefix())
		return nil
	}
	if err := r.halt(name); err != nil {
		return err
	}
	// Volumes go with the space. Holding them back kept the space dir alive, and
	// List enumerates spaces/ subdirs, so the space reappeared on every refresh
	// and no command could remove it. Volumes may be owned by a mapped subuid (a
	// container ran as non-root); escalate into a userns when a plain remove hits
	// EACCES/EPERM.
	return subidRemoveAll(r.scope(name).spaceDir())
}

// Stop halts a space's containers and network but keeps its state dir, so the
// named volumes — and the data in them — survive for the next `up`.
func (r *runcRegistry) Stop(ctx context.Context, name string) error {
	name = sanitizeSpaceName(name)
	if name == "" || !r.exists(name) {
		return spaceapi.ErrSpaceNotFound
	}
	return r.halt(name)
}

// halt is the teardown Down and Stop share: kill the init (reaping any straggler
// under this space's id prefix) and tear the space network down. What separates
// the two verbs is only what happens to the state dir afterwards.
func (r *runcRegistry) halt(name string) error {
	space, err := NewSpace(name)
	if err != nil {
		return err
	}
	space.Stop(spaceInitService)
	space.Close()
	return nil
}

// Exec runs a command (or an interactive shell) inside a live space and returns
// the command's exit code. ErrSpaceNotFound when the space does not exist;
// ErrSpaceNotRunning when its init container is not running. When
// opts.Container names a target, ErrContainerNotFound / ErrContainerNotRunning
// report that target's own state rather than the space's.
func (r *runcRegistry) Exec(ctx context.Context, name string, opts spaceapi.ExecOpts, io spaceapi.ExecIO) (int, error) {
	name = sanitizeSpaceName(name)
	if !r.exists(name) {
		return -1, spaceapi.ErrSpaceNotFound
	}
	// Exec must NOT go through NewSpace: that constructor reaps every container
	// under the space's id prefix (orphan cleanup for a fresh start), so building
	// one here killed the very container we are about to enter — every `exec`
	// into a dev/ci/deploy space died this way, and the caller then read the
	// corpse as "not running". Resolve the coordinates read-only instead.
	runcBin, runcRoot, err := execCoords(name)
	if err != nil {
		return -1, err
	}
	id := newScope(name, "").containerID(spaceInitService)
	if opts.Container != "" {
		target, terr := resolveContainerID(name, opts.Container)
		if terr != nil {
			return -1, terr
		}
		id = target
	} else {
		// Prefer the `space up` init (`shell`); fall back to the first running
		// container otherwise, so `exec` also works on spaces launched by dev/ci/
		// deploy (whose containers carry service names, not the init) — matching
		// what `ls` reports as running.
		if st := containerStatus(runcBin, runcRoot, id); st != "running" && st != "created" {
			raw := firstRunningContainerID(name)
			if raw == "" {
				return -1, spaceapi.ErrSpaceNotRunning
			}
			id = raw
		}
	}

	cmd := opts.Cmd
	if len(cmd) == 0 {
		cmd = interactiveShell(serviceFromContainerID(id, name))
	}
	if !opts.TTY {
		// Non-TTY exec (HTTP transport, piped CLI): stream stdio without a PTY.
		return RuncExecStreams(ctx, runcBin, runcRoot, id, cmd, io.Stdin, io.Stdout, io.Stdout)
	}
	return RuncExecTTY(ctx, runcBin, runcRoot, id, cmd,
		execPeer{in: io.Stdin, out: io.Stdout}, io.Rows, io.Cols)
}

// Purge tears down every space with no running container (the GC for the dev/ci
// leak), volumes included. Returns the names reaped.
func (r *runcRegistry) Purge(ctx context.Context) ([]string, error) {
	names, err := r.spaceNames()
	if err != nil {
		return nil, err
	}
	var reaped []string
	var errs error
	for _, name := range names {
		if r.hasRunningContainer(name) {
			continue
		}
		// Continue on failure: one stuck space (e.g. a subuid-owned volume the
		// operator cannot delete) must not freeze the GC for the rest.
		if err := r.Down(ctx, name); err != nil {
			errs = errors.Join(errs, fmt.Errorf("purge space %q: %w", name, err))
			continue
		}
		reaped = append(reaped, name)
	}
	return reaped, errs
}

// --- helpers --------------------------------------------------------------

func (r *runcRegistry) scope(name string) spaceScope { return newScope(name, r.cacheRoot) }

// exists reports whether a space's per-space state dir is present.
func (r *runcRegistry) exists(name string) bool {
	st, err := os.Stat(r.scope(name).spaceDir())
	return err == nil && st.IsDir()
}

// ListSpaceNames returns every named space under the host cache (the spaces/
// dir entries), sorted. It is the socketless, runc-free name discovery `space
// ls` uses before describing each space — exposed for shell completion so the
// completer never duplicates the cache-layout scan. A missing cache yields an
// empty list, not an error.
func ListSpaceNames() ([]string, error) {
	root, err := cacheRoot()
	if err != nil {
		return nil, err
	}
	return spaceNamesIn(spacesRoot(root))
}

// spaceNamesIn reads the directory entries of dir as space names, sorted. A
// missing dir yields an empty list, not an error.
func spaceNamesIn(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// spaceNames lists the named spaces under the cache (the spaces/ dir entries),
// sorted. A missing dir yields an empty list, not an error.
func (r *runcRegistry) spaceNames() ([]string, error) {
	return spaceNamesIn(spacesRoot(r.cacheRoot))
}

// describe builds a SpaceInfo from the runc state + filesystem for one space.
func (r *runcRegistry) describe(name string) (spaceapi.SpaceInfo, error) {
	containers, err := ListDeployContainers(name)
	if err != nil {
		return spaceapi.SpaceInfo{}, err
	}
	info := spaceapi.SpaceInfo{Name: name, Status: aggregateStatus(containers)}
	for _, c := range containers {
		info.Containers = append(info.Containers, spaceapi.ContainerInfo{ID: c.ID, Status: c.Status})
	}
	info.Volumes = r.volumeNames(name)
	info.Repo = r.spaceRepo(name)
	return info, nil
}

// spaceRepo recovers the host repo path bind-mounted at /workspace from one of
// the space's OCI bundles (config.json). Bundles live at
// <bundleRoot>/svc-*/config.json; the first whose mounts contain /workspace
// wins. Returns "" when no bundle exposes it (e.g. a bare `space up` with no
// repo mount, or bundles already reaped).
func (r *runcRegistry) spaceRepo(name string) string {
	scope := r.scope(name)
	entries, err := os.ReadDir(scope.bundleRoot())
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if repo := repoFromBundle(filepath.Join(scope.bundleRoot(), e.Name(), "config.json")); repo != "" {
			return repo
		}
	}
	return ""
}

// repoFromBundle reads an OCI config.json and returns the source path of the
// /workspace bind mount (the host repo), or "" if absent/unreadable.
func repoFromBundle(configPath string) string {
	b, err := os.ReadFile(configPath)
	if err != nil {
		return ""
	}
	var spec struct {
		Mounts []struct {
			Destination string `json:"destination"`
			Source      string `json:"source"`
		} `json:"mounts"`
	}
	if json.Unmarshal(b, &spec) != nil {
		return ""
	}
	for _, m := range spec.Mounts {
		if m.Destination == "/workspace" {
			return m.Source
		}
	}
	return ""
}

// interactiveShell is the default shell command for an interactive exec. It does
// two things a bare `sh` cannot:
//
// It names the container in the prompt. Every service of a space shares one image
// and one /workspace bind mount, so a bare `#` is identical in all of them:
// os-release, the file tree and the kernel all match, and only /proc/1/cmdline
// tells them apart — not something an operator should have to read to know where
// they are.
//
// It prefers bash -i when the image has it. `sh` is dash on Debian, which has no
// line editing: the arrow keys arrive as raw escape sequences, print as ^[[A, and
// leave the line in a state the user has to Ctrl-C out of. bash -i gives history
// and editing; the fallback keeps a usable shell on an image without bash (the
// prompt still names the container).
func interactiveShell(label string) []string {
	prompt := "# "
	if label != "" {
		prompt = "[" + label + "] # "
	}
	// --norc keeps /etc/bash.bashrc from resetting PS1 to its own value, so the
	// exported prompt survives. exec so the shell replaces this wrapper rather
	// than lingering as the parent of the session the user is typing into.
	return []string{"sh", "-c",
		"PS1='" + prompt + "'; export PS1; " +
			"if command -v bash >/dev/null 2>&1; then exec bash --norc -i; fi; exec sh"}
}

// serviceFromContainerID recovers the service name from a runc container id
// (`sklp-deploy-<space>-<svc>`), or "" when the id does not carry the prefix.
func serviceFromContainerID(id, spaceName string) string {
	prefix := newScope(spaceName, "").containerIDPrefix()
	if svc := strings.TrimPrefix(id, prefix); svc != id && svc != "" {
		return svc
	}
	return ""
}

// execCoords resolves the runc binary and a space's state root WITHOUT building
// a Space: NewSpace reaps the space's containers on construction, which is right
// for a fresh start and fatal for an exec into a live one.
func execCoords(spaceName string) (runcBin, runcRoot string, err error) {
	root, err := cacheRoot()
	if err != nil {
		return "", "", fmt.Errorf("space exec: cache root: %w", err)
	}
	runcBin, err = resolveRunc(root)
	if err != nil {
		return "", "", fmt.Errorf("space exec: runc: %w", err)
	}
	return runcBin, newScope(spaceName, root).runcStateRoot(), nil
}

// resolveContainerID maps a caller's container target onto a runc id that is
// actually running in the space. The target is accepted as a service name
// ("core") or as the full runc id ("sklp-deploy-<space>-core"), since the id is
// what `space get` prints and the service name is what an operator reads off it.
//
// A converged oneshot (migrate, seed) is a found-but-stopped target: it must be
// reported as such, not silently redirected to a running sibling, or the caller
// believes they landed in the container they asked for.
func resolveContainerID(spaceName, target string) (string, error) {
	containers, err := ListDeployContainers(spaceName)
	if err != nil {
		return "", err
	}
	return pickContainer(containers, spaceName, target)
}

// pickContainer is the pure selection resolveContainerID wraps: match the target
// against the space's containers and report why it cannot be entered when it
// cannot. Split out so the matching rules are testable without a live runc.
func pickContainer(containers []DeployContainerInfo, spaceName, target string) (string, error) {
	prefix := newScope(spaceName, "").containerIDPrefix()
	want := target
	if !strings.HasPrefix(want, prefix) {
		want = prefix + sanitize(target)
	}
	for _, c := range containers {
		if c.ID != want {
			continue
		}
		if c.Status != "running" && c.Status != "created" {
			return "", fmt.Errorf("%w: %s is %s", spaceapi.ErrContainerNotRunning, target, c.Status)
		}
		return c.ID, nil
	}
	return "", fmt.Errorf("%w: %s", spaceapi.ErrContainerNotFound, target)
}

// firstRunningContainerID returns the raw runc id of the space's first running
// container, or "" when none runs. The exec fallback target for spaces without
// the `space up` init (dev/ci/deploy launch service-named containers instead).
func firstRunningContainerID(name string) string {
	containers, err := ListDeployContainers(name)
	if err != nil {
		return ""
	}
	for _, c := range containers {
		if c.Status == "running" || c.Status == "created" {
			return c.ID
		}
	}
	return ""
}

// hasRunningContainer reports whether any of the space's containers is running.
func (r *runcRegistry) hasRunningContainer(name string) bool {
	containers, err := ListDeployContainers(name)
	if err != nil {
		return false
	}
	for _, c := range containers {
		if c.Status == "running" || c.Status == "created" {
			return true
		}
	}
	return false
}

// volumeNames lists the named volume directories materialised for a space
// (spaces/<name>/volumes/*), so down -v / get can show what data is at stake.
func (r *runcRegistry) volumeNames(name string) []string {
	entries, err := os.ReadDir(r.scope(name).volumesRoot())
	if err != nil {
		return nil
	}
	var vols []string
	for _, e := range entries {
		if e.IsDir() {
			vols = append(vols, e.Name())
		}
	}
	sort.Strings(vols)
	return vols
}

// aggregateStatus folds a space's container statuses into one: "running" if any
// container runs, "stopped" if state exists but nothing runs, "" if no state.
func aggregateStatus(containers []DeployContainerInfo) string {
	if len(containers) == 0 {
		return ""
	}
	for _, c := range containers {
		if c.Status == "running" || c.Status == "created" {
			return "running"
		}
	}
	return "stopped"
}

// supervisedSet returns the set of spaces with a live deploy control socket — a
// live socket means the space is a deploy, not a bare space. Best-effort: a nil
// lister or a listing failure yields an empty set (no rows flagged), never an
// error — the Supervised column is advisory.
func (r *runcRegistry) supervisedSet() map[string]bool {
	if r.listSupervised == nil {
		return map[string]bool{}
	}
	spaces, err := r.listSupervised()
	if err != nil {
		return map[string]bool{}
	}
	set := make(map[string]bool, len(spaces))
	for _, s := range spaces {
		set[sanitizeSpaceName(s)] = true
	}
	return set
}

// execPeer adapts the transport's split stdin/stdout into the single
// io.ReadWriter RuncExecTTY proxies against the container PTY.
type execPeer struct {
	in  io.Reader
	out io.Writer
}

func (p execPeer) Read(b []byte) (int, error)  { return p.in.Read(b) }
func (p execPeer) Write(b []byte) (int, error) { return p.out.Write(b) }

// RemoveEmptyShells deletes every space dir under the cache that holds no runc
// state, no volume, no bundle and no rootfs, the names in keep excepted, and
// returns the names removed.
func RemoveEmptyShells(keep []string) ([]string, error) {
	root, err := cacheRoot()
	if err != nil {
		return nil, err
	}
	return removeEmptyShellsIn(root, keep)
}

func removeEmptyShellsIn(cacheRoot string, keep []string) ([]string, error) {
	names, err := spaceNamesIn(spacesRoot(cacheRoot))
	if err != nil {
		return nil, err
	}
	kept := make(map[string]bool, len(keep))
	for _, k := range keep {
		kept[k] = true
	}
	var removed []string
	var errs error
	for _, name := range names {
		if kept[name] {
			continue
		}
		scope := newScope(name, cacheRoot)
		if !scope.isEmptyShellDir() {
			continue
		}
		if err := os.RemoveAll(scope.spaceDir()); err != nil {
			errs = errors.Join(errs, fmt.Errorf("remove empty space dir %q: %w", name, err))
			continue
		}
		removed = append(removed, name)
	}
	return removed, errs
}
