package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/lalternativefabrique/sklp/internal/cli"
)

// pauseCommand is the hidden `sklp pause` verb: sklp's own container-init keep-
// alive. The runner parks a container on it (bind-mounted or baked into the
// rootfs) and execs the real work in via `runc exec`, so parking never depends
// on the guest rootfs shipping `sleep`/`sh` — a minimal space.yaml rootfs
// (packages: git, curl, ... and no coreutils) has neither.
//
// Unlike `sleep 2147483647` it behaves as a correct PID 1: it reaps reparented
// orphans (an exec'd `sklp run` forks step processes whose orphans land here)
// and exits 0 on SIGTERM/SIGINT so teardown is graceful instead of a SIGKILL
// that would cut an attached exec/PTY mid-session.
func pauseCommand() cli.Command {
	return cli.Command{
		Name:   "pause",
		Short:  "Container-init keep-alive (internal)",
		Hidden: true,
		Run: func(ctx context.Context, args cli.Args) int {
			return runPause(ctx)
		},
	}
}

func runPause(ctx context.Context) int {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGCHLD)
	for {
		select {
		case <-ctx.Done():
			return 0
		case s := <-sigs:
			if s == syscall.SIGCHLD {
				// Reap every reparented orphan; a single SIGCHLD can coalesce many.
				for {
					pid, err := syscall.Wait4(-1, nil, syscall.WNOHANG, nil)
					if pid <= 0 || err != nil {
						break
					}
				}
				continue
			}
			return 0 // SIGTERM/SIGINT: graceful exit, no SIGKILL needed.
		}
	}
}
