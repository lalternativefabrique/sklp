package space

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"sync"

	"golang.org/x/sys/unix"
)

// runcEnters records, per runc state root, the nsenter prefix every runc
// invocation on that root must run behind. A space whose services live in a
// child user namespace registers the space user namespace here: its root is
// the operator, with capabilities over every child, so `runc state`, `kill`
// and `delete` reach containers whose files and processes belong to a subuid
// the operator otherwise cannot read or signal. Keyed by state root because
// that is the one value every runc helper already carries.
var (
	runcEntersMu sync.Mutex
	runcEnters   = map[string][]string{}
)

func setRuncEnter(runcRoot string, enter []string) {
	runcEntersMu.Lock()
	defer runcEntersMu.Unlock()
	if enter == nil {
		delete(runcEnters, runcRoot)
		return
	}
	runcEnters[runcRoot] = enter
}

func runcEnterFor(runcRoot string) []string {
	runcEntersMu.Lock()
	defer runcEntersMu.Unlock()
	return runcEnters[runcRoot]
}

func runcCommandContext(ctx context.Context, runcBin, runcRoot string, args ...string) *exec.Cmd {
	full := append([]string{"--root", runcRoot}, args...)
	if enter := runcEnterFor(runcRoot); enter != nil {
		return exec.CommandContext(ctx, "nsenter", append(append(append([]string{}, enter...), "--", runcBin), full...)...)
	}
	return exec.CommandContext(ctx, runcBin, full...)
}

func runcCommand(runcBin, runcRoot string, args ...string) *exec.Cmd {
	return runcCommandContext(context.Background(), runcBin, runcRoot, args...)
}

// killAs signals pid with the authority registered for runcRoot: a plain
// kill(2) when none, else kill(1) inside the space user namespace.
func killAs(runcRoot string, pid int, sig unix.Signal) error {
	enter := runcEnterFor(runcRoot)
	if enter == nil {
		return unix.Kill(pid, sig)
	}
	argv := append(append([]string{}, enter...), "--", "kill", "-s", unix.SignalName(sig)[3:], strconv.Itoa(pid))
	if out, err := exec.Command("nsenter", argv...).CombinedOutput(); err != nil {
		return fmt.Errorf("kill %d in space userns: %w: %s", pid, err, out)
	}
	return nil
}
