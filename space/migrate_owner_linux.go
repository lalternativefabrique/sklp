//go:build linux

package space

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/lalternativefabrique/sklp/space/ociproc"
	"github.com/lalternativefabrique/sklp/space/spacenet"
)

// MigrateVolumeOwner hands every named volume of this space from the
// operator to the space's child root, or back. Only files the operator owns
// move; a service's own uid keeps its files, since its mapping is the same in
// both namespaces. It is the one-time step before a space with data runs in
// a child user namespace, and must run with the space stopped: a start never
// touches data (see handNewVolumes). The volumes moved are returned.
func (s *Space) MigrateVolumeOwner(back bool) ([]string, error) {
	if s.scope.standalone() {
		return nil, fmt.Errorf("a standalone space has no child root")
	}
	slot, err := childRootSlot(s.cacheRoot, s.scope.name)
	if err != nil {
		return nil, err
	}
	id, err := ociproc.ResolveIdentity(os.Getuid(), os.Getgid())
	if err != nil {
		return nil, fmt.Errorf("delegated range: %w", err)
	}
	root, ok := id.SpaceRootOf(slot, childRootSlots)
	if !ok {
		return nil, fmt.Errorf("slot %d does not fit the delegated range (%d ids)", slot, id.UIDCount)
	}
	rootID := root.WorkerID
	from, to := 0, rootID
	if back {
		from, to = rootID, 0
	}
	entries, err := os.ReadDir(s.volumesRoot())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var moved []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(s.volumesRoot(), e.Name())
		if err := spacenet.RetargetTreeOwner(p, from, to); err != nil {
			return moved, fmt.Errorf("volume %s: %w", e.Name(), err)
		}
		moved = append(moved, e.Name())
	}
	return moved, nil
}
