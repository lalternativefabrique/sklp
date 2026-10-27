package space

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// volumeNameRE bounds a named volume to one path segment: "." and ".." would
// otherwise join out of volumesRoot.
var volumeNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// ValidateNamedVolume accepts a `volumes:` entry only in the named form
// "name:/ctr/path[:ro|rw]". A manifest describes an app, not the node: a host
// bind would hand the container any path the space user can read, and inside
// the userns container root IS that user (its 0600 files included). Host binds
// stay a runtime-internal facility for system-launched containers.
func ValidateNamedVolume(entry string) error {
	vm, err := parseVolume(entry, "/")
	if err != nil {
		return err
	}
	if !vm.named {
		return fmt.Errorf("invalid volume %q: host paths cannot be mounted; declare a named volume (\"name:/container/path\")", entry)
	}
	return nil
}

// Volumes are a space-level isolation primitive, not a dev convenience: now
// that a service rootfs is mounted read-only (see buildServiceSpec), an image
// that writes its data under a declared path (postgres PGDATA, nats store) must
// have that path backed by a writable mount, or it aborts at boot against the
// read-only rootfs. A volume resolves to an rw bind-mount the launcher folds
// into the service's DevMounts — so deploy and dev share one mechanism.

// volumeMount is a resolved `volumes:` entry: an absolute host directory bound
// rw at an absolute container Destination.
type volumeMount struct {
	hostPath    string
	destination string
	named       bool
	readOnly    bool
}

// parseVolume resolves one docker-style `-v` entry into a volumeMount:
//
//   - "/host/path:/ctr/path"      -> bind that (absolute) host path.
//   - "/host/file:/ctr/file:ro"   -> bind read-only (the ":ro" suffix).
//   - "name:/ctr/path"            -> a NAMED volume materialised as a stable
//     per-name directory under volumesRoot so it persists across runs.
//
// A named token is anything whose source is not absolute. The container
// destination must be absolute. It is pure (no FS); resolveVolumes does the
// mkdir.
func parseVolume(entry, volumesRoot string) (volumeMount, error) {
	src, rest, ok := strings.Cut(entry, ":")
	if !ok || src == "" || rest == "" {
		return volumeMount{}, fmt.Errorf("invalid volume %q: want <name|host>:/container/path", entry)
	}
	// Optional trailing ":ro" / ":rw" mode after the destination.
	dst, mode, hasMode := strings.Cut(rest, ":")
	readOnly := false
	if hasMode {
		switch mode {
		case "ro":
			readOnly = true
		case "rw":
			readOnly = false
		default:
			return volumeMount{}, fmt.Errorf("invalid volume %q: unknown mode %q (want ro|rw)", entry, mode)
		}
	}
	if dst == "" {
		return volumeMount{}, fmt.Errorf("invalid volume %q: want <name|host>:/container/path", entry)
	}
	if !strings.HasPrefix(dst, "/") {
		return volumeMount{}, fmt.Errorf("invalid volume %q: container path %q must be absolute", entry, dst)
	}
	if strings.HasPrefix(src, "/") {
		return volumeMount{hostPath: src, destination: dst, readOnly: readOnly}, nil // host bind
	}
	if !volumeNameRE.MatchString(src) {
		return volumeMount{}, fmt.Errorf("invalid volume name %q in %q: want %s", src, entry, volumeNameRE.String())
	}
	return volumeMount{hostPath: filepath.Join(volumesRoot, src), destination: dst, named: true, readOnly: readOnly}, nil
}

// resolveVolumes parses every declared volume into an rw DevMount, creating
// each host source directory (a runc bind-mount fails if its source is
// missing). Ownership is settled afterwards by Space.handNewVolumes. A single
// bad entry fails the whole set, so a service never launches with a silently
// dropped volume writing into the read-only rootfs instead.
func resolveVolumes(entries []string, volumesRoot string) ([]DevMount, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	out := make([]DevMount, 0, len(entries))
	for _, e := range entries {
		vm, err := parseVolume(e, volumesRoot)
		if err != nil {
			return nil, err
		}
		// An absolute host source that already exists as a FILE is a file bind
		// (e.g. /etc/resolv.conf:/etc/resolv.conf:ro) — never mkdir over it.
		// Otherwise materialise the directory (a runc bind fails on a missing
		// source). Named volumes are always directories.
		if st, err := os.Stat(vm.hostPath); err == nil && !st.IsDir() {
			// existing file: bind as-is, no mkdir/chown.
		} else {
			mode := fs.FileMode(0o755)
			if vm.named {
				mode = 0o700
			}
			if err := os.MkdirAll(vm.hostPath, mode); err != nil {
				return nil, fmt.Errorf("volume %q: create host dir %q: %w", e, vm.hostPath, err)
			}
		}
		out = append(out, DevMount{Source: vm.hostPath, Destination: vm.destination, ReadOnly: vm.readOnly})
	}
	return out, nil
}

// anonImageVolumes backs every image-config VOLUME with a per-space emptyDir,
// the way docker auto-creates an anonymous volume — UNLESS the path is already
// covered by an explicit mount (a declared volumes: entry or a dev mount), which
// takes precedence. Without this, an image VOLUME (postgres' PGDATA, nats' store)
// lands on the read-only rootfs and the entrypoint aborts at boot. The host dir
// is keyed deterministically by service + container path so it is stable across
// runs within the space. svcName scopes it so two services with the same VOLUME
// path don't collide.
func anonImageVolumes(svcName string, imgVols map[string]struct{}, existing []DevMount, volumesRoot string) ([]DevMount, error) {
	if len(imgVols) == 0 {
		return nil, nil
	}
	covered := make(map[string]struct{}, len(existing))
	for _, m := range existing {
		covered[filepath.Clean(m.Destination)] = struct{}{}
	}
	// Deterministic order: sort the paths so the result is stable.
	paths := make([]string, 0, len(imgVols))
	for p := range imgVols {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	out := make([]DevMount, 0, len(paths))
	for _, p := range paths {
		dst := filepath.Clean(p)
		if !strings.HasPrefix(dst, "/") {
			continue // ignore malformed VOLUME entries
		}
		if _, ok := covered[dst]; ok {
			continue // an explicit mount wins
		}
		host := filepath.Join(volumesRoot, "_anon-"+envKeyify(svcName)+"-"+envKeyify(dst))
		if err := os.MkdirAll(host, 0o700); err != nil {
			return nil, fmt.Errorf("image volume %q: create host dir %q: %w", p, host, err)
		}
		out = append(out, DevMount{Source: host, Destination: dst})
	}
	return out, nil
}

func ensurePrivateDir(path string) error {
	return os.MkdirAll(path, 0o700)
}
