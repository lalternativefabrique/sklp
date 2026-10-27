package sklet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/lalternativefabrique/sklp/space"
	"github.com/lalternativefabrique/sklp/spacemanager"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
)

// Down tears a space down for good, in the order that avoids a parasitic
// restart or a reboot resurrection, whatever the supervisor mode:
//
//  1. systemd: DISABLE the unit FIRST (disable --now = stop + disable + remove
//     the unit file). Disabling before teardown stops Restart=on-failure from
//     relaunching the supervisor mid-teardown, and dropping the unit stops it
//     coming back at boot.
//  2. daemonless: ask the supervisor to stop itself via POST /v1/down (cleanest
//     — it acks then tears down on its own ctx); fall back to SIGTERM via the
//     pidfile if the socket does not answer.
//  3. id-based teardown as a safety net (reap any container the supervisor left).
//  4. remove the persisted manifest + per-space artifacts so a reboot does NOT
//     resurrect a torn-down space — done LAST, only after the stop above, so a
//     half-failed down never loses a still-running space's manifest.
//
// It writes feedback/warnings to out/errOut and returns an error instead of
// calling os.Exit. Idempotent: a down on an already-down / never-deployed space
// is a clear no-op message, not a crash.
//
// This is the LOCAL teardown: it acts on this host's runc state and config dir.
// A remote `sklp deploy delete` only relays POST /v1/down to the node, which
// runs this same path from inside its own supervisor.
func Down(meta spacemanager.SpaceMeta, services []spacemanager.Service, out, errOut io.Writer) error {
	return DownWithVolumes(meta, services, false, out, errOut)
}

// DownWithVolumes is Down with the choice Down never offered: volumes=true
// destroys the space's named volumes along with it, irreversibly. Kept volumes
// hold the space dir alive whenever they live under it, and the inventory
// lists spaces by directory name — which is how a deleted space came back as a
// stopped ghost no command could remove.
func DownWithVolumes(meta spacemanager.SpaceMeta, services []spacemanager.Service, volumes bool, out, errOut io.Writer) error {
	spaceName := meta.Name
	stopped := false

	if control.SystemdAvailable() {
		// disable --now stops + disables + removes the unit: no restart, no boot.
		_ = control.DisableUnit(spaceName)
		stopped = true
	}

	// Daemonless-detached supervisor (or a foreground/systemd one still serving):
	// prefer the clean inside-out stop over an external signal. Down acks 202 then
	// the supervisor tears down on its own ctx; WaitGone confirms the socket is
	// gone. Fall back to SIGTERM (pidfile) when the socket does not answer.
	if downViaSocketWithVolumes(spaceName, volumes) {
		stopped = true
	} else if sig, err := control.SignalSupervisorDown(spaceName); err != nil {
		fmt.Fprintf(errOut, "sklet: warning: could not signal background supervisor for space %q: %v\n", spaceName, err)
	} else if sig {
		stopped = true
	}

	// Safety net: reap any instance the supervisor left running (best-effort).
	if err := TeardownSpace(spaceName, services); err != nil {
		return err
	}

	// Reclaim the space's whole cache dir (overlay scratch, deploy-bundles,
	// runc-state) — everything PurgeSpaceScratch alone left behind (a
	// permanent leak on every prior down/delete). Named volumes go only when
	// the caller asked (`-v`): kept, they also keep the space dir, which the
	// inventory reads as a space that still exists. Best-effort: a leftover
	// only wastes disk.
	if err := space.PurgeSpaceCache(spaceName, !volumes); err != nil {
		fmt.Fprintf(errOut, "sklet: warning: could not purge cache for %q: %v\n", spaceName, err)
	}

	// Drop the persisted manifest LAST so a reboot cannot resurrect the space.
	if err := control.RemoveSpaceArtifacts(spaceName); err != nil {
		fmt.Fprintf(errOut, "sklet: warning: could not remove space artifacts for %q: %v\n", spaceName, err)
	}

	if stopped {
		fmt.Fprintf(out, "space %q torn down (%d service(s))\n", spaceName, len(services))
	} else {
		// No unit, no live socket, no pidfile: nothing was supervising it. The
		// id-based teardown still ran, but the space was effectively not running.
		fmt.Fprintf(out, "space %q is not running (torn down any leftover containers)\n", spaceName)
	}
	return nil
}

// DownViaSocket asks a live supervisor to stop itself over the control socket
// (POST /v1/down) and waits for the socket to disappear. Returns true only when
// a supervisor answered AND confirmed the stop. ErrNoSupervisor (no socket) is a
// clean false (let the caller fall back to the pidfile signal).
func DownViaSocket(spaceName string) bool {
	return downViaSocketWithVolumes(spaceName, false)
}

func downViaSocketWithVolumes(spaceName string, volumes bool) bool {
	client, err := control.Dial(spaceName)
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	down := client.DownAndPurge
	if volumes {
		down = client.DownAndDelete
	}
	if err := down(ctx); err != nil {
		if errors.Is(err, control.ErrNoSupervisor) {
			return false // no live supervisor: caller tries the pidfile
		}
		fmt.Fprintf(os.Stderr, "sklet: warning: control down for space %q: %v\n", spaceName, err)
		return false
	}
	// Acked: wait for the supervisor's teardown to close/remove the socket.
	wctx, wcancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer wcancel()
	if err := client.WaitGone(wctx); err != nil {
		fmt.Fprintf(os.Stderr, "sklet: warning: space %q supervisor accepted stop but socket still answers: %v\n", spaceName, err)
	}
	return true
}
