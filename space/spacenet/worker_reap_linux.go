//go:build linux

package spacenet

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// workerProc is one scanned process seen through the lens the worker reaper
// needs: its pid, its parent pid, and whether its environment carries the space
// worker markers. Split from procEntry (the slirp reaper's argv-based view) so
// each reaper scans only what it needs and stays independently testable.
type workerProc struct {
	pid      int
	ppid     int
	isWorker bool // environ has SKLP_SPACE_WORKER= and SKLP_SPACE_BRIDGE=
}

// reapOrphanWorkers kills every orphaned space worker: a re-exec'd sklp process
// carrying the worker env markers whose parent is gone (reparented to PID 1).
// Pdeathsig reaps the worker on a clean supervisor death, but a worker forked
// before its Pdeathsig was armed (the clone/exec window) can still be orphaned
// by a kill -9; this is the startup backstop that guarantees the next `sklp dev`
// starts clean. Killing a worker destroys its netns, taking the space bridge and
// every veth with it; its holder/peer children die via their own Pdeathsig.
// Best-effort. It NEVER kills a worker whose parent is still alive (still owned).
func reapOrphanWorkers() {
	procs, err := listWorkerProcs()
	if err != nil {
		return // best-effort: no /proc access => nothing to reap
	}
	for _, victim := range orphanWorkerPIDs(procs) {
		_ = syscall.Kill(victim, syscall.SIGKILL)
	}
}

// orphanWorkerPIDs is the pure selector: from all processes, return the pids of
// space workers that have been reparented to PID 1 (parent died). A live parent
// means the worker is still owned by a running supervisor, so it is left alone.
// Side-effect-free so a test drives it with synthetic procs.
func orphanWorkerPIDs(procs []workerProc) []int {
	var victims []int
	for _, p := range procs {
		if !p.isWorker {
			continue
		}
		// ppid==1 is the orphan signal: the forking supervisor is gone and init
		// adopted us. A self-pid==1 guard is defensive (we never run as init).
		if p.ppid == 1 && p.pid > 1 {
			victims = append(victims, p.pid)
		}
	}
	return victims
}

// listWorkerProcs scans /proc for every process, reading its parent pid from
// stat and its worker-marker presence from environ. A process whose environ is
// unreadable (a foreign uid) is reported isWorker=false: we cannot have forked
// it, so it is not ours to reap.
func listWorkerProcs() ([]workerProc, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var out []workerProc
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // non-pid entry
		}
		ppid, ok := procPPID(pid)
		if !ok {
			continue // gone between readdir and stat, or unparsable
		}
		out = append(out, workerProc{pid: pid, ppid: ppid, isWorker: procIsWorker(pid)})
	}
	return out, nil
}

// procPPID returns the parent pid from /proc/<pid>/stat. The format is
// "pid (comm) state ppid ...": ppid is the field after state, but comm may
// contain spaces or parens, so we anchor on the LAST ')' and take fields after.
func procPPID(pid int) (int, bool) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, false
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 || i+2 >= len(s) {
		return 0, false
	}
	fields := strings.Fields(s[i+2:]) // [state, ppid, pgrp, ...]
	if len(fields) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, false
	}
	return ppid, true
}

// procIsWorker reports whether pid's environment carries BOTH space worker
// markers. We require both (not just SKLP_SPACE_WORKER) so an unrelated process
// that merely inherited one var cannot be mistaken for a worker. environ is
// NUL-delimited; an unreadable environ (foreign uid / gone) is treated as
// not-a-worker, which is safe — we only ever reap processes we could have forked.
func procIsWorker(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil || len(b) == 0 {
		return false
	}
	var hasWorker, hasBridge bool
	for _, kv := range strings.Split(string(b), "\x00") {
		switch {
		case strings.HasPrefix(kv, workerEnv+"="):
			hasWorker = true
		case strings.HasPrefix(kv, envBridge+"="):
			hasBridge = true
		}
	}
	return hasWorker && hasBridge
}
