//go:build linux

package spacenet

import "syscall"

// reap_public_linux.go exposes the orphan reapers as a single public entry
// point for `sklp clean`. The individual reapers stay private (they are
// startup backstops called internally); this wraps them for the explicit,
// user-driven prune path and adds a dry-run that reports victims without killing.

// OrphanProcReapResult reports the orphan processes a prune pass reaped (or, in
// dry-run, would reap): egress slirp4netns instances and re-exec'd space workers
// whose owner is gone.
type OrphanProcReapResult struct {
	SlirpKilled  []int
	WorkerKilled []int
}

// ReapOrphanProcs kills (or, when dryRun, only lists) every orphaned slirp4netns
// and space worker — those whose owner pid/parent is dead. It reuses the exact
// same pure selectors the startup backstops use, so the dry-run preview matches
// what a real prune would remove. It NEVER selects a process whose owner is
// still alive.
func ReapOrphanProcs(dryRun bool) OrphanProcReapResult {
	var res OrphanProcReapResult

	if procs, err := listProcs(); err == nil {
		for _, victim := range orphanSlirpPIDs(procs, pidAlive) {
			res.SlirpKilled = append(res.SlirpKilled, victim)
			if !dryRun {
				_ = syscall.Kill(victim, syscall.SIGKILL)
			}
		}
	}
	if procs, err := listWorkerProcs(); err == nil {
		for _, victim := range orphanWorkerPIDs(procs) {
			res.WorkerKilled = append(res.WorkerKilled, victim)
			if !dryRun {
				_ = syscall.Kill(victim, syscall.SIGKILL)
			}
		}
	}
	return res
}
