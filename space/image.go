package space

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/containerd/v2/pkg/archive"
	"github.com/containerd/containerd/v2/pkg/archive/compression"
	"github.com/distribution/reference"
	"github.com/lalternativefabrique/sklp/space/spacenet"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// newImageResolver builds the containerd registry resolver used to pull image
// manifests + blobs. It authenticates from the sklp registries file and treats a
// LOCAL registry (localhost / 127.0.0.1 / *.local) as plain HTTP: a dev-built
// image is pushed to a local registry that serves HTTP, which the default
// HTTPS-only resolver rejects ("server gave HTTP response to HTTPS client").
// Remote registries are unaffected — they stay HTTPS.
func newImageResolver(ctx context.Context) remotes.Resolver {
	authFn := registryAuths(registryAuthsFrom(ctx))
	return docker.NewResolver(docker.ResolverOptions{
		Hosts: docker.ConfigureDefaultRegistries(
			docker.WithAuthorizer(docker.NewDockerAuthorizer(
				docker.WithAuthCreds(authFn),
			)),
			docker.WithPlainHTTP(docker.MatchLocalhost),
		),
	})
}

// normalizeRef expands a short image reference to the fully-qualified form
// containerd's resolver requires: "alpine:3.20" -> "docker.io/library/
// alpine:3.20", "postgres:16" -> "docker.io/library/postgres:16". Docker does
// this implicitly, but the low-level containerd resolver does not — a
// host-less ref makes it build "dummy://alpine:3.20" and fail on the ":3.20".
// A ref that is already fully qualified (with a registry host) passes through.
func normalizeRef(ref string) (string, error) {
	named, err := reference.ParseDockerRef(ref)
	if err != nil {
		return "", fmt.Errorf("parse image ref %q: %w", ref, err)
	}
	return named.String(), nil
}

// refCacheDir holds tag→digest pointers so an already-pulled image can be
// reused without re-contacting the registry. Registries rate-limit anonymous
// manifest GETs (Docker Hub 429), so a deploy of a known image must NOT need
// the network — this is the cache-only path, mirroring the runner's -no-pull.
const refCacheDir = "refs"

// sanitizeRef turns a fully-qualified ref into a single filesystem-safe file
// name (slashes/colons → underscores) for the tag→digest pointer file.
func sanitizeRef(ref string) string {
	r := strings.NewReplacer("/", "_", ":", "_", "@", "_")
	return r.Replace(ref)
}

// readCachedDigest returns the digest a prior pull recorded for ref, or "" if
// none. The pointer is only trusted when its rootfs is still extracted (checked
// by the caller), so a pruned cache falls back to the network cleanly.
func readCachedDigest(cacheRoot, ref, arch string) string {
	digest, _ := readCachedPointer(cacheRoot, refCacheKey(ref, arch))
	return digest
}

// readCachedPointer reads a refs/ file as (digest, repository). Files written
// before the repository was recorded hold the bare digest and yield an empty
// repository — the collector treats those as ungrouped rather than guessing,
// since sanitizeRef maps both "/" and ":" to "_" and the split point is not
// recoverable from the file name.
func readCachedPointer(cacheRoot, key string) (string, string) {
	b, err := os.ReadFile(filepath.Join(cacheRoot, refCacheDir, key))
	if err != nil {
		return "", ""
	}
	digest, repo, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	return strings.TrimSpace(digest), strings.TrimSpace(repo)
}

// refCacheKey qualifies the ref by arch: the recorded digest is the PICKED
// platform manifest, so a node of another arch must re-resolve, not reuse it.
func refCacheKey(ref, arch string) string {
	return sanitizeRef(ref) + "-" + arch
}

// writeCachedDigest records ref→digest after a successful resolve so the next
// run of the same tag skips the registry entirely. Best-effort: a write error
// only costs a future re-resolve, never correctness.
//
// The repository is recorded on a second line because the collector keeps the
// newest generations PER SERVICE, and the file name cannot answer which
// repository a pointer belongs to: sanitizeRef maps "/" and ":" alike to "_".
func writeCachedDigest(cacheRoot, ref, arch, digest string) {
	dir := filepath.Join(cacheRoot, refCacheDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	body := digest
	if repo := refRepository(ref); repo != "" {
		body += "\n" + repo
	}
	_ = os.WriteFile(filepath.Join(dir, refCacheKey(ref, arch)), []byte(body), 0o644)
}

// refRepository is a ref stripped of its tag and digest — the identity of the
// service across generations, which is what the per-service retention groups
// on. It returns "" for a ref that does not parse, which leaves the pointer
// ungrouped rather than lumping unrelated images together.
func refRepository(ref string) string {
	named, err := reference.ParseDockerRef(ref)
	if err != nil {
		return ""
	}
	return named.Name()
}

// imageConfig is the subset of the OCI image config we need to derive a
// container's default process args when a service does not override them.
type imageConfig struct {
	Config struct {
		Entrypoint []string `json:"Entrypoint"`
		Cmd        []string `json:"Cmd"`
		Env        []string `json:"Env"`
		WorkingDir string   `json:"WorkingDir"`
		// Volumes is the OCI image-config VOLUME set ({"/var/lib/x":{}}); each
		// needs a writable backing since the rootfs is read-only.
		Volumes map[string]struct{} `json:"Volumes"`
		// User is the OCI image-config USER ("101", "nginx", "101:101", "nginx:nginx").
		// Used to chown named/anon volume host dirs so the container process can
		// write into them without a chown syscall at runtime.
		User string `json:"User"`
	} `json:"config"`
}

// BaseConfig is the subset of a base image's OCI config the application-image
// builder inherits as defaults when a Dockerfile does not override them, matching
// Docker's FROM-inheritance semantics (Env/WorkingDir/User/Entrypoint/Cmd).
type BaseConfig struct {
	Env        []string
	WorkingDir string
	User       string
	Entrypoint []string
	Cmd        []string
}

// PrepareImageWithEnv is PrepareImage plus the base image's OCI Env (the
// application-image builder needs the base's PATH so `go`/`node` resolve in a
// RUN). Returns the rootfs dir and the image config's Env (KEY=value), or an
// empty Env if the config could not be fetched (the rootfs is still usable).
func PrepareImageWithEnv(ctx context.Context, cacheRoot, ref string) (string, []string, error) {
	rootfs, cfg, err := prepareServiceImage(ctx, cacheRoot, ref, runtime.GOARCH)
	if err != nil {
		return "", nil, err
	}
	return rootfs, cfg.Config.Env, nil
}

// PrepareImageWithConfig is PrepareImage plus the base image's inheritable OCI
// config defaults (Env, WorkingDir, User, Entrypoint, Cmd). The builder seeds
// each stage's Result with these so a Dockerfile that omits CMD/ENTRYPOINT
// inherits the base's, the way `docker build` does.
func PrepareImageWithConfig(ctx context.Context, cacheRoot, ref string) (string, BaseConfig, error) {
	return PrepareImageWithConfigFor(ctx, cacheRoot, ref, runtime.GOARCH)
}

// PrepareImageWithConfigFor is PrepareImageWithConfig for an explicit
// architecture: the application-image builder pulls the base of the platform
// it targets, not of the host it runs on.
func PrepareImageWithConfigFor(ctx context.Context, cacheRoot, ref, arch string) (string, BaseConfig, error) {
	rootfs, cfg, err := prepareServiceImage(ctx, cacheRoot, ref, arch)
	if err != nil {
		return "", BaseConfig{}, err
	}
	return rootfs, BaseConfig{
		Env:        cfg.Config.Env,
		WorkingDir: cfg.Config.WorkingDir,
		User:       cfg.Config.User,
		Entrypoint: cfg.Config.Entrypoint,
		Cmd:        cfg.Config.Cmd,
	}, nil
}

// PrepareImage is the exported entry to prepareImage for in-tree callers (the
// runner-image Go builder) that need a base rootfs extracted without a daemon.
// It returns the path to the extracted rootfs dir, cached across runs.
func PrepareImage(ctx context.Context, cacheRoot, ref string) (string, error) {
	return prepareImage(ctx, cacheRoot, ref, runtime.GOARCH)
}

// prepareImage pulls a manifest, downloads the missing layers, and
// extracts them into <cacheRoot>/rootfs/<digest>/. Cached on subsequent
// runs: a sentinel `.extracted` file marks completed extractions.
//
// Multi-arch images: we pick the linux/<arch> entry deterministically.
func prepareImage(ctx context.Context, cacheRoot, ref, arch string) (string, error) {
	ref, err := normalizeRef(ref)
	if err != nil {
		return "", err
	}
	// Cache-only fast path: if a prior pull recorded this tag's digest AND that
	// rootfs is still extracted, return it without touching the registry. This
	// is what makes a re-deploy of a known image survive a registry rate-limit.
	if dg := readCachedDigest(cacheRoot, ref, arch); dg != "" {
		rootfs := filepath.Join(cacheRoot, "rootfs", dg)
		if _, err := os.Stat(filepath.Join(rootfs, ".extracted")); err == nil {
			return rootfs, nil
		}
	}
	resolver := newImageResolver(ctx)
	name, desc, err := resolver.Resolve(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", ref, err)
	}
	fetcher, err := resolver.Fetcher(ctx, name)
	if err != nil {
		return "", err
	}
	body, err := readAll(ctx, fetcher, desc)
	if err != nil {
		return "", err
	}
	var manifest ocispec.Manifest
	if desc.MediaType == ocispec.MediaTypeImageIndex ||
		desc.MediaType == "application/vnd.docker.distribution.manifest.list.v2+json" {
		var index ocispec.Index
		if err := json.Unmarshal(body, &index); err != nil {
			return "", err
		}
		picked, err := pickLinux(index, ref, arch)
		if err != nil {
			return "", err
		}
		body, err = readAll(ctx, fetcher, picked)
		if err != nil {
			return "", err
		}
		desc = picked
		if err := json.Unmarshal(body, &manifest); err != nil {
			return "", err
		}
	} else {
		if err := json.Unmarshal(body, &manifest); err != nil {
			return "", err
		}
	}

	blobDir := filepath.Join(cacheRoot, "blobs")
	if err := os.MkdirAll(blobDir, 0o755); err != nil {
		return "", err
	}
	// Record tag→digest now that the registry resolved it, so the next run of
	// this tag can take the cache-only fast path above.
	writeCachedDigest(cacheRoot, ref, arch, desc.Digest.Encoded())
	return materializeRootfs(cacheRoot, desc.Digest.Encoded(), func(staging string) error {
		owners := layerOwners{}
		for _, layer := range manifest.Layers {
			path := filepath.Join(blobDir, layer.Digest.Encoded())
			if err := fetchLayerBlob(ctx, fetcher, layer, path); err != nil {
				return err
			}
			if err := applyLayerBlob(ctx, staging, path, owners); err != nil {
				return err
			}
		}
		return restoreLayerOwners(staging, owners)
	})
}

// layerOwners is the container uid:gid each extracted path was declared with,
// the last layer writing a path winning, as it does for its content.
type layerOwners map[string][2]int

// restoreLayerOwners gives back the ownership the rootless extraction had to
// drop. An image that runs as www-data expects its bootstrap/cache to be
// www-data's; flattened to root, Laravel cannot write it and Invoice Ninja
// never boots. A host without subuid delegation keeps the flattened tree, as
// before, and says so.
func restoreLayerOwners(rootfs string, owners layerOwners) error {
	var list strings.Builder
	for p, ids := range owners {
		if ids[0] == 0 && ids[1] == 0 {
			continue
		}
		fmt.Fprintf(&list, "%d %d %s\n", ids[0], ids[1], p)
	}
	if list.Len() == 0 {
		return nil
	}
	f, err := os.CreateTemp(filepath.Dir(rootfs), ".owners-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(list.String()); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := spacenet.ApplyOwnerList(rootfs, f.Name()); err != nil {
		if errors.Is(err, os.ErrPermission) {
			fmt.Fprintf(os.Stderr, "[sklp] image files keep the operator as owner: %v\n", err)
			return nil
		}
		return err
	}
	return nil
}

// fetchLayerBlob downloads one layer to path unless a blob of that digest is
// already there. The bytes are checked against the digest as they land: a
// download cut short by the registry or by a killed process leaves nothing
// behind, so the next pull cannot extract a truncated archive as if it were
// the layer.
func fetchLayerBlob(ctx context.Context, fetcher remotes.Fetcher, layer ocispec.Descriptor, path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	rc, err := fetcher.Fetch(ctx, layer)
	if err != nil {
		return err
	}
	defer rc.Close()
	tmp := path + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	verifier := layer.Digest.Verifier()
	_, err = io.Copy(io.MultiWriter(f, verifier), rc)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && !verifier.Verified() {
		err = fmt.Errorf("layer %s: downloaded bytes do not match its digest", layer.Digest.Encoded())
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// applyLayerBlob extracts one downloaded layer into rootfs and removes the
// tarball it came from.
//
// The archive is spent once applied: nothing reads blobs/ again, because a
// cached image returns on the .extracted sentinel before the blob directory is
// ever consulted. Keeping them accumulated one archive per layer per
// generation forever — 29.7G on a production node, more than the extracted
// rootfs trees they fed. A layer shared with a later image is re-fetched
// rather than kept; the registry serves it and the disk does not grow either
// way.
//
// Removal is best-effort: a spent tarball that cannot be unlinked costs disk,
// never correctness, and must not fail an extraction that already succeeded.
func applyLayerBlob(ctx context.Context, rootfs, path string, owners layerOwners) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	dc, err := compression.DecompressStream(f)
	if err != nil {
		return err
	}
	defer dc.Close()
	filter := func(h *tar.Header) (bool, error) {
		keep, err := rootlessLayerFilter(h)
		if keep && err == nil && owners != nil {
			owners[filepath.Clean(h.Name)] = [2]int{h.Uid, h.Gid}
		}
		return keep, err
	}
	if _, err := archive.Apply(ctx, rootfs, dc, archive.WithNoSameOwner(), archive.WithFilter(filter)); err != nil {
		return fmt.Errorf("apply layer %s: %w", filepath.Base(path), err)
	}
	_ = os.Remove(path)
	return nil
}

// schilyXattrPrefix is the PAX record key prefix under which tar carries
// extended attributes (containerd's archive package reads these to setxattr the
// extracted file). A layer entry's security.capability lives at
// "SCHILY.xattr.security.capability".
const schilyXattrPrefix = "SCHILY.xattr."

// rootlessLayerFilter is a tar Apply filter that makes containerd's archive.Apply
// survive a rootless (child user namespace) extraction. It handles the three
// privileged file operations the kernel refuses without CAP_* in the INITIAL
// user ns, each of which would otherwise abort the whole "apply layer" with
// "operation not permitted":
//
//  1. Device nodes / FIFOs — mknod(S_IFCHR|S_IFBLK) needs CAP_MKNOD in the
//     initial userns (e.g. mysql:8.0 bakes /dev/null, /dev/console, ...). The
//     runtime rebuilds a minimal /dev at create time, so these are noise: drop
//     them.
//
//  2. security.* / trusted.* file capabilities — setxattr of a security.*
//     namespace xattr needs CAP_SYS_ADMIN in the initial userns (a binary like
//     caddy carries security.capability = cap_net_bind_service). We can't write
//     it rootless, and we don't need to: the service process runs as the mapped
//     container-root with the cap already in its bounding set, so it binds :80
//     via the userns cap, not the file-cap. Strip the xattr; keep the file.
//
//  3. Read-only directories that still contain files — some images bake 0555
//     dirs (notably a Go module cache `go` marks read-only). A rootless
//     extraction creates the dir 0555 then cannot write the files inside it.
//     Force the owner write bit; the rootfs is mounted read-only at run time
//     anyway (buildServiceSpec), so the original protection is restored where it
//     matters.
//
//  4. Files the owner cannot even read — /etc/gshadow and /etc/shadow- are
//     baked 0000 by the shadow package. Extracting one is fine; a LATER layer
//     rewriting the same path is not, because the extractor opens the existing
//     file for writing and 0000 denies its owner too. Give the owner rw on
//     those alone: a 0444 file rewrites without help and is left as it is.
func rootlessLayerFilter(h *tar.Header) (bool, error) {
	switch h.Typeflag {
	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		return false, nil // can't mknod in a rootless userns; /dev is rebuilt at runtime
	case tar.TypeDir:
		h.Mode |= 0o200 // u+w
	case tar.TypeReg:
		// Only the fully unwritable ones: a 0444 file extracts and is rewritten
		// fine, but 0000 (what the shadow package bakes for /etc/gshadow) denies
		// even its owner, so a later layer rewriting that path fails to open it.
		if h.Mode&0o400 == 0 {
			h.Mode |= 0o600 // u+rw
		}
	}
	// Drop security.*/trusted.* xattrs we cannot setxattr without CAP_SYS_ADMIN
	// in the initial userns. user.* xattrs are kept (allowed in a userns).
	for k := range h.PAXRecords {
		if !strings.HasPrefix(k, schilyXattrPrefix) {
			continue
		}
		attr := k[len(schilyXattrPrefix):]
		if strings.HasPrefix(attr, "security.") || strings.HasPrefix(attr, "trusted.") {
			delete(h.PAXRecords, k)
		}
	}
	return true, nil
}

// prepareServiceImage pulls + unpacks a service image AND fetches its
// config blob, so the orchestrator can derive default process args
// (entrypoint+cmd) and env when the service declares no Command. The
// rootfs extraction reuses prepareImage; the config fetch is a second,
// cheap blob read on the already-resolved manifest.
func prepareServiceImage(ctx context.Context, cacheRoot, ref, arch string) (rootfs string, cfg imageConfig, err error) {
	rootfs, err = prepareImage(ctx, cacheRoot, ref, arch)
	if err != nil {
		return "", imageConfig{}, err
	}
	// The image config is cached by digest alongside the rootfs, so the
	// cache-only path (registry rate-limited) still yields default args/env.
	// rootfs is <cacheRoot>/rootfs/<digest>; reuse that digest as the key.
	digest := filepath.Base(rootfs)
	cfg, err = fetchImageConfig(ctx, cacheRoot, ref, digest, arch)
	if err != nil {
		return "", imageConfig{}, fmt.Errorf("image config %s: %w", ref, err)
	}
	return rootfs, cfg, nil
}

// fetchImageConfig returns the image's OCI config. It first tries the on-disk
// cache (<cacheRoot>/configs/<digest>.json) so a known image needs no network;
// on a miss it resolves the ref, reads the config blob, and caches it.
func fetchImageConfig(ctx context.Context, cacheRoot, ref, digest, arch string) (imageConfig, error) {
	cfgPath := filepath.Join(cacheRoot, "configs", digest+".json")
	if b, rerr := os.ReadFile(cfgPath); rerr == nil {
		var cfg imageConfig
		if json.Unmarshal(b, &cfg) == nil {
			return cfg, nil
		}
	}
	ref, err := normalizeRef(ref)
	if err != nil {
		return imageConfig{}, err
	}
	resolver := newImageResolver(ctx)
	name, desc, err := resolver.Resolve(ctx, ref)
	if err != nil {
		return imageConfig{}, fmt.Errorf("resolve %s: %w", ref, err)
	}
	fetcher, err := resolver.Fetcher(ctx, name)
	if err != nil {
		return imageConfig{}, err
	}
	body, err := readAll(ctx, fetcher, desc)
	if err != nil {
		return imageConfig{}, err
	}
	var manifest ocispec.Manifest
	if desc.MediaType == ocispec.MediaTypeImageIndex ||
		desc.MediaType == "application/vnd.docker.distribution.manifest.list.v2+json" {
		var index ocispec.Index
		if err := json.Unmarshal(body, &index); err != nil {
			return imageConfig{}, err
		}
		picked, err := pickLinux(index, ref, arch)
		if err != nil {
			return imageConfig{}, err
		}
		body, err = readAll(ctx, fetcher, picked)
		if err != nil {
			return imageConfig{}, err
		}
		if err := json.Unmarshal(body, &manifest); err != nil {
			return imageConfig{}, err
		}
	} else {
		if err := json.Unmarshal(body, &manifest); err != nil {
			return imageConfig{}, err
		}
	}
	cfgBody, err := readAll(ctx, fetcher, manifest.Config)
	if err != nil {
		return imageConfig{}, err
	}
	var cfg imageConfig
	if err := json.Unmarshal(cfgBody, &cfg); err != nil {
		return imageConfig{}, err
	}
	// Cache the raw config by digest so a later cache-only run reuses it.
	if err := os.MkdirAll(filepath.Join(cacheRoot, "configs"), 0o755); err == nil {
		_ = os.WriteFile(cfgPath, cfgBody, 0o644)
	}
	return cfg, nil
}

// pickLinux selects the linux/<arch> manifest from a multi-arch index
// deterministically, erroring when none is present.
func pickLinux(index ocispec.Index, ref, arch string) (ocispec.Descriptor, error) {
	for _, m := range index.Manifests {
		if m.Platform != nil && m.Platform.OS == "linux" && m.Platform.Architecture == arch {
			return m, nil
		}
	}
	return ocispec.Descriptor{}, fmt.Errorf("no linux/%s manifest in index for %s", arch, ref)
}

func readAll(ctx context.Context, fetcher interface {
	Fetch(context.Context, ocispec.Descriptor) (io.ReadCloser, error)
}, desc ocispec.Descriptor) ([]byte, error) {
	rc, err := fetcher.Fetch(ctx, desc)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// imageUser resolves an OCI image User field ("70", "70:70", "postgres",
// "postgres:postgres") to the ids the process runs as. A name is looked up in
// the image's own etc/passwd and etc/group, as runc would; a bare name takes
// the account's primary group. known is false when a name is not in the
// image, in which case the ids fall back to root.
func imageUser(rootfs, user string) (uid, gid int, known bool) {
	if user == "" {
		return 0, 0, true
	}
	userPart, groupPart, hasGroup := strings.Cut(user, ":")
	known = true
	if n, err := strconv.Atoi(userPart); err == nil {
		uid = n
	} else if entry, ok := lookupPasswd(rootfs, userPart); ok {
		uid, gid = entry.uid, entry.gid
	} else {
		known = false
	}
	if !hasGroup {
		return uid, gid, known
	}
	if n, err := strconv.Atoi(groupPart); err == nil {
		return uid, n, known
	}
	if g, ok := lookupGroup(rootfs, groupPart); ok {
		return uid, g, known
	}
	return uid, 0, false
}

type passwdEntry struct{ uid, gid int }

func lookupPasswd(rootfs, name string) (passwdEntry, bool) {
	for _, f := range readColonFile(filepath.Join(rootfs, "etc", "passwd")) {
		if len(f) < 4 || f[0] != name {
			continue
		}
		uid, err1 := strconv.Atoi(f[2])
		gid, err2 := strconv.Atoi(f[3])
		if err1 == nil && err2 == nil {
			return passwdEntry{uid: uid, gid: gid}, true
		}
	}
	return passwdEntry{}, false
}

func lookupGroup(rootfs, name string) (int, bool) {
	for _, f := range readColonFile(filepath.Join(rootfs, "etc", "group")) {
		if len(f) < 3 || f[0] != name {
			continue
		}
		if gid, err := strconv.Atoi(f[2]); err == nil {
			return gid, true
		}
	}
	return 0, false
}

func readColonFile(path string) [][]string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out [][]string
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, strings.Split(line, ":"))
	}
	return out
}

// InstallLocalImage publishes a locally-built rootfs into the image store under
// ref, so a deploy of that tag resolves through the cache-only fast path in
// prepareImage instead of the registry. It is what makes `push: false` usable:
// without it a built image has no name the runtime can resolve, and a host-less
// ref normalises to docker.io/<ref>, which 401s.
//
// srcRootfs is copied, not moved: the caller's build work dir is removed when
// the build returns.
func InstallLocalImage(cacheRoot, ref, digest string, configJSON []byte, srcRootfs string) error {
	normalized, err := normalizeRef(ref)
	if err != nil {
		return err
	}
	// The pull path keys rootfs/ and configs/ by the BARE hex digest; an
	// "sha256:"-prefixed key writes a directory no overlay lowerdir will find.
	digest = strings.TrimPrefix(digest, "sha256:")
	// prepareServiceImage resolves the rootfs from cache but then reads the
	// image config from configs/<digest>.json — and goes to the registry when it
	// is missing, which for a never-pushed ref is a 401. Installing the rootfs
	// without the config only moves the failure one step later.
	if len(configJSON) > 0 {
		cfgDir := filepath.Join(cacheRoot, "configs")
		if err := os.MkdirAll(cfgDir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(cfgDir, digest+".json"), configJSON, 0o644); err != nil {
			return fmt.Errorf("write image config: %w", err)
		}
	}
	if _, err := materializeRootfs(cacheRoot, digest, func(staging string) error {
		if err := os.Remove(staging); err != nil {
			return err
		}
		if err := copyTree(srcRootfs, staging); err != nil {
			return fmt.Errorf("copy rootfs: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}
	writeCachedDigest(cacheRoot, normalized, runtime.GOARCH, digest)
	return nil
}

// copyTree copies a directory tree, preserving mode and symlinks. Ownership is
// not preserved: a rootless build already produced the tree as the host uid,
// which is what the runtime expects to bind-mount read-only.
func copyTree(src, dst string) error {
	// A rootless build writes the rootfs through a uid map, so an image that
	// chowns to a non-root user (`chown postgres:postgres`, or an apk package
	// shipping /etc/postgresql17) leaves directories owned by a subuid this
	// process is not. Ownership — not mode — is what denies the walk below, so
	// take the tree over first, then widen the modes a build may have narrowed.
	// Each entry is still copied with its own recorded mode.
	if err := subidChownTree(src); err != nil {
		return err
	}
	if err := makeTraversable(src); err != nil {
		return err
	}
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case info.IsDir():
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case !info.Mode().IsRegular():
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm()|0o200)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}

// makeTraversable widens every directory under root to u+rwx so the copy walk
// can read it, whatever uid the rootless build left it owned by. It cannot use
// filepath.Walk: a directory missing u+x has to be chmod'd BEFORE anything can
// descend into it, which is exactly what Walk would need to do first. Chmod
// answers to ownership rather than to the mode, so widening works even where
// reading does not. A directory we genuinely cannot chmod is left alone; the
// copy walk reports it.
func makeTraversable(dir string) error {
	if info, err := os.Lstat(dir); err == nil && info.IsDir() && info.Mode().Perm()&0o700 != 0o700 {
		_ = os.Chmod(dir, info.Mode().Perm()|0o700)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsPermission(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if e.Type()&os.ModeSymlink != 0 {
			continue
		}
		if err := makeTraversable(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}
