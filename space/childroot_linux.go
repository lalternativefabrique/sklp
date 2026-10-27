//go:build linux

package space

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/lalternativefabrique/sklp/space/spacenet"
)

// childRootSlots is how many ids at the top of the delegated range are set
// aside as space roots: space with slot k has root at id count-k.
const childRootSlots = spacenet.ChildRootSlots

// childRootSlot returns the space's slot, allocating the lowest free one on
// first use. The allocation is node-wide and persistent
// (<cacheRoot>/child-roots.json), so a space keeps its root uid, and with it
// the files it owns, across restarts.
func childRootSlot(cacheRoot, space string) (int, error) {
	if space == "" {
		return 0, fmt.Errorf("child userns: a standalone space has no root slot")
	}
	path := filepath.Join(cacheRoot, "child-roots.json")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return 0, err
	}
	slots := map[string]int{}
	if raw, err := os.ReadFile(path); err == nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, &slots); err != nil {
			return 0, fmt.Errorf("child root slots %q: %w", path, err)
		}
	}
	if slot, ok := slots[space]; ok {
		return slot, nil
	}
	used := map[int]bool{}
	for _, s := range slots {
		used[s] = true
	}
	slot := 0
	for used[slot] {
		slot++
	}
	if slot >= childRootSlots {
		return 0, fmt.Errorf("child root slots exhausted (%d spaces)", childRootSlots)
	}
	slots[space] = slot
	raw, _ := json.MarshalIndent(slots, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return 0, err
	}
	return slot, os.Rename(tmp, path)
}
