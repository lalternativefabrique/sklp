//go:build linux

package spacenet

import (
	"os/exec"
	"runtime"
)

// startPinned runs cmd.Start() on a goroutine locked to its OS thread that is
// never unlocked. This is mandatory for any cmd whose SysProcAttr sets
// Pdeathsig: the kernel arms the parent-death signal against the THREAD that
// issued the clone, not the process. Go's runtime may destroy an idle worker
// thread, which would fire Pdeathsig and reap the child while the supervisor is
// still alive. Pinning the cloning goroutine to a thread that lives forever (no
// UnlockOSThread) keeps that thread alive for the process lifetime, so the
// signal only ever fires on real supervisor death.
//
// The Start runs on a dedicated goroutine so the caller's goroutine is not
// itself pinned (and burned) for the child's whole lifetime. cmd must be fully
// configured before the call; the returned error is Start's.
func startPinned(cmd *exec.Cmd) error {
	errCh := make(chan error, 1)
	go func() {
		runtime.LockOSThread() // never unlocked: this thread must outlive the child
		errCh <- cmd.Start()
		// Park the pinned thread for the process lifetime. The goroutine holding
		// the lock must stay alive, else the runtime frees the thread and Pdeathsig
		// fires early. select{} blocks without spinning.
		if cmd.Process != nil {
			select {}
		}
	}()
	return <-errCh
}
