//go:build linux

package space

import (
	"os"
	"strconv"
	"strings"

	"github.com/lalternativefabrique/sklp/space/spacenet"
)

func cgroupPIDs(containerID string) []int {
	return spacenet.CgroupProcsFor(containerID)
}

// processTree returns root and every process descending from it. Inside a pid
// namespace an orphan reparents to that namespace's init, so the tree under a
// container's init is exactly its process set, whatever cgroup runc used.
func processTree(root int) []int {
	children := map[int][]int{}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return []int{root}
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if ppid, ok := parentPID(pid); ok {
			children[ppid] = append(children[ppid], pid)
		}
	}
	tree := []int{root}
	for i := 0; i < len(tree); i++ {
		tree = append(tree, children[tree[i]]...)
	}
	return tree
}

func parentPID(pid int) (int, bool) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	// The comm field may contain spaces; the parent pid follows the closing paren.
	rest := string(raw)
	if i := strings.LastIndexByte(rest, ')'); i >= 0 {
		rest = rest[i+1:]
	}
	fields := strings.Fields(rest)
	if len(fields) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(fields[1])
	return ppid, err == nil
}
