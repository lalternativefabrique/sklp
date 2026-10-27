// run_serve_linux.go implements `sklp run --listen`: the node-side API that executes
// pipelines on request. It is what turns a VM into a CI node — sklp already
// carries the engine, this exposes it over the network.
//
// The node identity (self-signed cert + host-wide Bearer token) is the SAME
// one `sklp deploy` uses, so a machine has one credential no matter which
// plane a caller reaches. The startup banner prints what a client needs to
// connect, mirroring what the deploy supervisor prints under --listen.
//
// Linux-only because spacemanager/control is: the node identity helpers live
// in its _linux files, and a CI node runs rootless spaces, which are Linux-only
// anyway. run_serve_other.go keeps the verb present elsewhere with a message
// saying so, rather than letting the whole CLI fail to build.
package main

import (
	"fmt"
	"os"

	"github.com/lalternativefabrique/sklp/pipeline/runserve"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
)

// nodeIdentitySpace keys the persisted cert/token. The run API is host-wide,
// not per-space, but control's helpers still take the argument.
const nodeIdentitySpace = "production"

// runServeInstall installs the run API as a systemd --user service and returns
// without blocking. A CI node is shelved and unshelved twice a day, so an API
// started in the foreground is gone after the first cycle.
func runServeInstall(o listenOptions) int {
	if !control.SystemdAvailable() {
		fmt.Fprintln(os.Stderr, "sklp run --install: no usable systemd --user session here")
		return 1
	}
	selfExe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp run --install: locate binary:", err)
		return 1
	}
	// Without lingering the unit stops at logout and never starts at boot,
	// which defeats the point — but it may need polkit, so a failure is a
	// warning, not a stop.
	if lerr := control.EnableLinger(); lerr != nil {
		fmt.Fprintln(os.Stderr, "sklp run --install: warning: could not enable lingering:", lerr)
	}
	if err := control.EnsureRunAPIUnit(selfExe, control.RunAPIOptions(o)); err != nil {
		fmt.Fprintln(os.Stderr, "sklp run --install:", err)
		return 1
	}
	fmt.Printf("installed %s — the run API now starts at boot and restarts on failure\n", control.RunAPIUnitName)
	fmt.Printf("  status: systemctl --user status %s\n", control.RunAPIUnitName)
	fmt.Printf("  logs:   journalctl --user -u %s -f\n", control.RunAPIUnitName)
	return 0
}

// runServe starts the node run API on addr and blocks. advertise, when set, is
// the host a client should dial — printed in the banner so the operator does
// not have to work out the VM's public address themselves.
func runServe(o listenOptions) int {
	addr, advertise, workRoot, concurrency := o.Addr, o.Advertise, o.WorkRoot, o.Concurrency
	if workRoot != "" {
		if err := os.MkdirAll(workRoot, 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "sklp run --listen: work root:", err)
			return 1
		}
	}
	cert, fingerprint, err := control.EnsureServerCert(nodeIdentitySpace)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp run --listen: server certificate:", err)
		return 1
	}
	token, err := control.EnsureToken(nodeIdentitySpace)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp run --listen: node token:", err)
		return 1
	}

	host := advertise
	if host == "" {
		host = "<node-host>"
	}
	fmt.Printf("sklp run API listening on %s\n", addr)
	fmt.Printf("  server:         https://%s%s\n", host, addr)
	// Named by its 0600 file, never printed: this is a pod log, collected and
	// retained far beyond the process that wrote it.
	fmt.Printf("  token file:     %s\n", control.TokenPath(nodeIdentitySpace))
	fmt.Printf("  ca-fingerprint: %s\n", fingerprint)
	if concurrency > 1 {
		fmt.Printf("  concurrency:    %d\n", concurrency)
	}
	if o.AllowHostSteps {
		fmt.Println("  host steps:     ALLOWED — a `space: false` pipeline runs on this node; serve trusted refs only")
	}

	srv := &runserve.Server{Token: token, WorkRoot: workRoot, Version: version, Concurrency: concurrency, AllowHostSteps: o.AllowHostSteps}
	if err := srv.Serve(addr, cert); err != nil {
		fmt.Fprintln(os.Stderr, "sklp run --listen:", err)
		return 1
	}
	return 0
}
