//go:build linux

package ociproc

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Identity is who a service container is on the host: the host ids container
// id 0 and the delegated range (container ids 1..Count) map to. It is the one
// definition of the container→host id translation; the worker is mapped from
// it, the OCI spec carries Mappings(), and a volume is owned by HostUIDOf().
type Identity struct {
	HostUID, HostGID   int
	UIDBase, GIDBase   int
	UIDCount, GIDCount int
	// RootPoolUID, RootPoolGID and RootPoolCount are a second delegated
	// range, disjoint from the first, from which the roots of child-userns
	// spaces are taken. With it a child maps every id 1..UIDCount an image
	// may run as, 65532 (nonroot) and 65534 (nobody) included; the worker
	// maps it at UIDCount+1.
	RootPoolUID, RootPoolGID, RootPoolCount int
}

// ResolveIdentity reads the caller's delegated subuid/subgid range. A non-root
// caller uses its /etc/subuid and /etc/subgid grants. Root inside a user
// namespace has no grant to read but may already own a range: whatever ids
// above 0 its own namespace maps are handed down. Root with nothing mapped
// above 0 gets the single 0→0 identity.
func ResolveIdentity(hostUID, hostGID int) (Identity, error) {
	if hostUID == 0 {
		uidCount, gidCount := mappedRangeAboveRoot(uidMapPath), mappedRangeAboveRoot(gidMapPath)
		if uidCount > 0 && gidCount > 0 {
			return Identity{UIDBase: 1, GIDBase: 1, UIDCount: uidCount, GIDCount: gidCount}, nil
		}
		return Identity{}, nil
	}
	name, err := currentSubIDName(hostUID)
	if err != nil {
		return Identity{}, err
	}
	uidBase, uidCount, err := readSubIDRange(subuidPath, name, hostUID)
	if err != nil {
		return Identity{}, err
	}
	gidBase, gidCount, err := readSubIDRange(subgidPath, name, hostUID)
	if err != nil {
		return Identity{}, err
	}
	id := Identity{HostUID: hostUID, HostGID: hostGID, UIDBase: uidBase, GIDBase: gidBase, UIDCount: uidCount, GIDCount: gidCount}
	poolUID, poolUIDCount := secondSubIDRange(subuidPath, name, hostUID, uidBase, uidCount)
	poolGID, poolGIDCount := secondSubIDRange(subgidPath, name, hostUID, gidBase, gidCount)
	if poolUIDCount > 0 && poolGIDCount > 0 {
		count := min(uidCount, gidCount)
		id.UIDCount, id.GIDCount = count, count
		id.RootPoolUID, id.RootPoolGID, id.RootPoolCount = poolUID, poolGID, min(poolUIDCount, poolGIDCount)
	}
	return id, nil
}

// SpaceRoot is where the root of the child user namespace in slot lives:
// WorkerID is its id in the space worker's namespace, HostUID/HostGID its
// ids on the host, and ChildCount how many ids 1..N the child maps. Taken
// from the root pool when the node has one wide enough, so an image keeps
// every id up to 65535; otherwise carved from the top of the range, which
// leaves an image only the ids below the carve.
type SpaceRoot struct {
	WorkerID, HostUID, HostGID, ChildCount int
}

// SpaceRootOf returns the root for slot out of slots; ok is false when the
// slot does not fit.
func (id Identity) SpaceRootOf(slot, slots int) (SpaceRoot, bool) {
	if slot < 0 || slot >= slots {
		return SpaceRoot{}, false
	}
	total := min(id.UIDCount, id.GIDCount)
	if id.RootPoolCount >= slots {
		return SpaceRoot{WorkerID: total + 1 + slot, HostUID: id.RootPoolUID + slot, HostGID: id.RootPoolGID + slot, ChildCount: total}, true
	}
	root := total - slot
	count := total - slots
	if count < 1 {
		return SpaceRoot{}, false
	}
	return SpaceRoot{WorkerID: root, HostUID: id.UIDBase + root - 1, HostGID: id.GIDBase + root - 1, ChildCount: count}, true
}

// WorkerIDOf is the id host uid hostUID has in the space worker's namespace.
func (id Identity) WorkerIDOf(hostUID int) int {
	if id.RootPoolCount > 0 && hostUID >= id.RootPoolUID && hostUID < id.RootPoolUID+id.RootPoolCount {
		return min(id.UIDCount, id.GIDCount) + 1 + hostUID - id.RootPoolUID
	}
	return hostUID - id.UIDBase + 1
}

// SingleIdentity maps only container id 0, onto the caller: the shape the Go
// runtime can write itself when no setuid helper is available.
func SingleIdentity(hostUID, hostGID int) Identity {
	return Identity{HostUID: hostUID, HostGID: hostGID}
}

// Mappings renders the identity as OCI uid/gid mappings.
func (id Identity) Mappings() (uid, gid []map[string]any) {
	if id.UIDCount == 0 || id.GIDCount == 0 {
		uid = []map[string]any{{"containerID": 0, "hostID": id.HostUID, "size": 1}}
		gid = []map[string]any{{"containerID": 0, "hostID": id.HostGID, "size": 1}}
		return uid, gid
	}
	return IDMappingsWithSubIDs(id.HostUID, id.HostGID, id.UIDBase, id.UIDCount, id.GIDBase, id.GIDCount)
}

// HostUIDOf is the host uid container uid containerUID runs as: 0 (or a
// symbolic USER the caller could not resolve, passed as 0) is HostUID, N>0 is
// UIDBase+N-1. ok is false for an id the identity does not map at all; such a
// uid owns nothing on the host and nothing may be handed to it.
func (id Identity) HostUIDOf(containerUID int) (uid int, ok bool) {
	if containerUID <= 0 {
		return id.HostUID, true
	}
	if containerUID > id.UIDCount {
		return 0, false
	}
	return id.UIDBase + containerUID - 1, true
}

// EnterArgs is the nsenter argv prefix that puts runc inside the user (and,
// with mountNS, mount) namespace of pid with the authority of that namespace's
// root. A caller that is mapped as uid 0 there keeps its credentials as they
// are; a caller mapped elsewhere, or not at all (it would be nobody), has to
// become uid 0 of the entered namespace, which the setns grant allows. An
// unreadable map falls back to keeping the credentials.
func EnterArgs(pid int, mountNS bool) []string {
	return enterArgs(pid, mountNS, fmt.Sprintf("/proc/%d/uid_map", pid), os.Getuid())
}

func enterArgs(pid int, mountNS bool, uidMap string, callerUID int) []string {
	args := []string{"-t", strconv.Itoa(pid), "-U"}
	if mountNS {
		args = append(args, "--mount")
	}
	inner, mapped := innerUID(uidMap, callerUID)
	if _, readable := rootHostUID(uidMap); readable && (!mapped || inner != 0) {
		return append(args, "-S", "0", "-G", "0")
	}
	return append(args, "--preserve-credentials")
}

// innerUID is the id hostUID has inside the namespace whose uid_map (as seen
// from the caller's namespace) is at path; mapped is false when it has none.
func innerUID(path string, hostUID int) (inner int, mapped bool) {
	for _, m := range readUIDMap(path) {
		if m.host <= hostUID && hostUID < m.host+m.size {
			return m.inner + hostUID - m.host, true
		}
	}
	return 0, false
}

// rootHostUID is the host uid container id 0 maps to; ok is false when the
// map is unreadable or has no line covering id 0.
func rootHostUID(path string) (int, bool) {
	for _, m := range readUIDMap(path) {
		if m.inner <= 0 && 0 < m.inner+m.size {
			return m.host - m.inner, true
		}
	}
	return 0, false
}

type idMapLine struct{ inner, host, size int }

func readUIDMap(path string) []idMapLine {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []idMapLine
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 3 {
			continue
		}
		inner, err1 := strconv.Atoi(fields[0])
		host, err2 := strconv.Atoi(fields[1])
		size, err3 := strconv.Atoi(fields[2])
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		out = append(out, idMapLine{inner, host, size})
	}
	return out
}
