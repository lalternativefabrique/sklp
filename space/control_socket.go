package space

import "path/filepath"

// ControlMountPoint is where a granted service finds its control socket, as
// ControlSocketName (ADR 0002).
const (
	ControlMountPoint = "/run/sklp"
	ControlSocketName = "control.sock"
)

// ControlSocketDir is the host directory bind-mounted at ControlMountPoint in
// a granted service. It lives in the space's own state dir, which only the
// space's child root may traverse (0710, its group): runc opens the mount
// source from inside the space's user namespace, and no other space or host
// account gets through.
func ControlSocketDir(spaceName, service string) (string, error) {
	root, err := cacheRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(newScope(spaceName, root).spaceDir(), "control", service), nil
}
