//go:build linux

package plan

import (
	"syscall"
	"testing"
)

// TestApplyParentDeathSignal guards the anti-orphan fix: every sandboxed child
// must carry Pdeathsig=SIGKILL so the kernel reaps it when the parent (a killed
// `go test ./runner/` or `sklp run`) dies — otherwise its namespaced
// descendants reparent to PID 1 and pile up (we leaked 1000+ once). A
// regression that drops this would silently bring the leak back.
func TestApplyParentDeathSignal(t *testing.T) {
	attr := &syscall.SysProcAttr{}
	applyParentDeathSignal(attr)
	if attr.Pdeathsig != syscall.SIGKILL {
		t.Fatalf("Pdeathsig = %v, want SIGKILL", attr.Pdeathsig)
	}
}

// TestSandboxAttrHasParentDeathSignal asserts the attr the sandbox launcher
// builds keeps BOTH Setpgid (clean group teardown) and Pdeathsig (brutal-kill
// reaping). It mirrors how runSandboxed composes the attr so a future edit that
// reorders/overwrites it is caught.
func TestSandboxAttrHasParentDeathSignal(t *testing.T) {
	attr := &syscall.SysProcAttr{Setpgid: true}
	applyParentDeathSignal(attr)
	applyNetworkIsolation(attr, sandboxSpec{Network: "none"})
	if !attr.Setpgid {
		t.Error("Setpgid lost")
	}
	if attr.Pdeathsig != syscall.SIGKILL {
		t.Error("Pdeathsig lost — orphan leak would return")
	}
	if attr.Cloneflags&syscall.CLONE_NEWNET == 0 {
		t.Error("network isolation lost")
	}
}
