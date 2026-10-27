//go:build linux

package spacenet

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
)

// TestOrphanWorkerPIDs drives the pure orphan selector: only processes flagged
// as workers AND reparented to PID 1 are reaped. A worker with a live parent is
// still owned; a non-worker orphan is none of our business.
func TestOrphanWorkerPIDs(t *testing.T) {
	procs := []workerProc{
		// Orphan worker: marker present, ppid==1 -> reap.
		{pid: 100, ppid: 1, isWorker: true},
		// Owned worker: marker present but parent still alive -> keep.
		{pid: 101, ppid: 4242, isWorker: true},
		// Orphan, but not a worker (e.g. a stray daemon adopted by init) -> keep.
		{pid: 102, ppid: 1, isWorker: false},
		// Worker whose parent is some live pid -> keep.
		{pid: 103, ppid: 50, isWorker: true},
		// Defensive: pid 1 itself is never a victim even if mislabelled.
		{pid: 1, ppid: 1, isWorker: true},
	}

	got := orphanWorkerPIDs(procs)
	if len(got) != 1 || got[0] != 100 {
		t.Fatalf("orphanWorkerPIDs = %v, want [100]", got)
	}

	// Sanity: no workers reparented to init -> nothing reaped.
	none := []workerProc{
		{pid: 200, ppid: 9, isWorker: true},
		{pid: 201, ppid: 1, isWorker: false},
	}
	if got := orphanWorkerPIDs(none); len(got) != 0 {
		t.Fatalf("no-orphan: orphanWorkerPIDs = %v, want []", got)
	}
}

// TestWorkerProcScan_Real exercises the /proc-reading predicates against a real
// child carrying the worker env markers: listWorkerProcs must see it as a worker
// with our pid as parent, and (parent alive) the reaper must NOT kill it. This
// covers procIsWorker + procPPID + the "owned worker is spared" guarantee on a
// live kernel; the orphan (ppid==1) branch is covered by the pure selector test.
func TestWorkerProcScan_Real(t *testing.T) {
	// A sleeper that carries both markers, so procIsWorker matches it exactly as
	// it would a real re-exec'd worker. It is our direct child, so its ppid is us.
	child := exec.Command("sleep", "30")
	child.Env = append(os.Environ(), workerEnv+"=2", envBridge+"=cap-test-br")
	if err := child.Start(); err != nil {
		t.Fatalf("start marked child: %v", err)
	}
	defer func() {
		_ = child.Process.Kill()
		_, _ = child.Process.Wait()
	}()
	pid := child.Process.Pid

	procs, err := listWorkerProcs()
	if err != nil {
		t.Fatalf("listWorkerProcs: %v", err)
	}
	var found *workerProc
	for i := range procs {
		if procs[i].pid == pid {
			found = &procs[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("marked child pid %d not found in /proc scan", pid)
	}
	if !found.isWorker {
		t.Errorf("marked child pid %d not detected as a worker", pid)
	}
	if found.ppid != os.Getpid() {
		t.Errorf("marked child ppid = %d, want %d (us)", found.ppid, os.Getpid())
	}

	// Parent (us) is alive, so the selector must spare it.
	for _, v := range orphanWorkerPIDs(procs) {
		if v == pid {
			t.Fatalf("reaper selected owned worker pid %d (parent alive)", pid)
		}
	}

	// And a real reap pass must leave it running.
	reapOrphanWorkers()
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("owned worker pid %d was killed by reapOrphanWorkers: %v", pid, err)
	}
}
