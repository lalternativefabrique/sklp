package devstack

// `sklp dev down` — out-of-process teardown of a running dev stack.
//
// Whereas Ctrl-C on the foreground `sklp dev` runs supervisor.stopAll
// against in-memory *proc handles, `down` is a fresh process that
// reconstructs the same teardown from the on-disk statefile (see
// state.go): SIGTERM the supervisor + every process group, wait the
// grace window, SIGKILL stragglers, then `docker stop` the run's OCI
// containers and remove its bridge network.

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// RunDown is the `sklp dev down` entrypoint. Returns an exit code so
// the caller (cmd/sklp) owns process lifecycle, matching Main.
func RunDown(args []string) int {
	fs := flag.NewFlagSet("sklp dev down", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	repo := fs.String("repo", ".", "repo root whose dev stack to stop")
	graceMs := fs.Int("grace-ms", 5000, "milliseconds to wait between SIGTERM and SIGKILL")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	repoAbs, err := filepath.Abs(*repo)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sklp dev down: resolve -repo: %v\n", err)
		return 2
	}

	// Stack is "" here: Step 4 wires a `-stack` flag through to scope the
	// statefile per-stack; until then `down` targets the legacy single-stack
	// run (devScopeID with no stack folded in).
	st, err := readState(repoAbs, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "sklp dev down: %v\n", err)
		return 1
	}
	if st == nil {
		fmt.Println("sklp dev down: no running dev stack found")
		return 0
	}

	grace := time.Duration(*graceMs) * time.Millisecond
	stopState(st, grace)
	removeState(repoAbs, "")
	fmt.Println("sklp dev down: stopped")
	return 0
}

// stopState replays the recorded teardown. Order mirrors the
// foreground path: signal the supervisor first (it may still be alive
// and able to shut itself down cleanly), then sweep process groups as
// a fallback, then OCI containers, then the network — a network can't
// be removed while endpoints are still attached.
func stopState(st *runState, grace time.Duration) {
	// 1. Ask the supervisor to shut down gracefully. If it's alive it
	//    runs shutdownOnce itself (which also removes the statefile);
	//    if it's already dead this is a no-op and the pgid sweep below
	//    does the real work.
	if st.SupervisorPID > 0 {
		_ = syscall.Kill(st.SupervisorPID, syscall.SIGTERM)
	}

	// Embedded run: the supervisor SIGTERM above triggers its Close (every
	// runc container + the space torn down). If the supervisor already died,
	// reap the space's orphaned containers + bridge as a fallback. No pgid /
	// OCI sweep applies — the runc launcher owns those containers.
	if st.Embedded {
		if st.SupervisorPID > 0 && !pidDead(st.SupervisorPID) && grace > 0 {
			waitPidGone(st.SupervisorPID, grace)
		}
		reapEmbeddedRun(st.EmbeddedSpace)
		return
	}

	// 2. SIGTERM every process group, wait the grace window once for
	//    all of them in parallel, then SIGKILL anything still alive.
	for _, pgid := range st.PGIDs {
		if pgid > 0 {
			_ = syscall.Kill(-pgid, syscall.SIGTERM)
		}
	}
	if grace > 0 {
		deadline := time.After(grace)
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			if allDead(st.PGIDs) {
				break
			}
			select {
			case <-deadline:
				goto kill
			case <-tick.C:
			}
		}
	}
kill:
	for _, pgid := range st.PGIDs {
		if pgid > 0 && !pgidDead(pgid) {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
	}

	// 3. Stop OCI containers with the runtime recorded at launch.
	if st.OCIRuntime != "" {
		rt := &ociRuntime{Name: st.OCIRuntime}
		for _, name := range st.OCIContainers {
			runOCIStop(ociStopHook{name: name, runtime: rt})
		}
	}

	// 4. Remove the bridge network last (after every container detaches).
	if st.Network != "" && st.OCIRuntime != "" {
		(&runNetwork{rt: &ociRuntime{Name: st.OCIRuntime}, name: st.Network}).destroy()
	}
}

// pidDead reports whether a single pid is gone (signal-0 existence check).
func pidDead(pid int) bool {
	return syscall.Kill(pid, syscall.Signal(0)) == syscall.ESRCH
}

// waitPidGone polls until the supervisor pid exits or the grace window lapses,
// so the embedded reap fallback only fires once the supervisor's own graceful
// Close had a chance to run.
func waitPidGone(pid int, grace time.Duration) {
	deadline := time.After(grace)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if pidDead(pid) {
			return
		}
		select {
		case <-deadline:
			return
		case <-tick.C:
		}
	}
}

// pgidDead reports whether a process group has no live members. We
// probe with signal 0 (existence check, no delivery) against -pgid;
// ESRCH means the whole group is gone.
func pgidDead(pgid int) bool {
	err := syscall.Kill(-pgid, syscall.Signal(0))
	return err == syscall.ESRCH
}

// allDead is true when every pgid in the slice reports dead.
func allDead(pgids []int) bool {
	for _, pgid := range pgids {
		if pgid > 0 && !pgidDead(pgid) {
			return false
		}
	}
	return true
}
