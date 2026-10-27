package cockpit

import (
	"strings"
	"testing"
)

func vmWithProcs() VMState {
	return VMState{
		Kernel:   "7.1.8-arch1-3",
		CPUs:     20,
		MemTotal: 30 << 30,
		MemAvail: 23 << 30,
		Processes: []VMProcess{
			{PID: 771525, Name: "node", RSS: 737 << 20},
			{PID: 6084, Name: "claude", RSS: 692 << 20},
			{PID: 10010, Name: "brave", RSS: 478 << 20},
		},
	}
}

// The process list is collapsed by default: it ranks the whole machine by
// memory, which on a workstation is the browser, not the cockpit's workload.
func TestVMProcessesCollapsedByDefault(t *testing.T) {
	m := &Model{VM: vmWithProcs(), Width: 120, Height: 40}
	out := Render(m, NewPlainRenderer())

	if strings.Contains(out, "brave") {
		t.Fatalf("process list must be hidden by default:\n%s", out)
	}
	if !strings.Contains(out, "3 processes") || !strings.Contains(out, "[V] show") {
		t.Fatalf("collapsed panel must offer to expand:\n%s", out)
	}
}

// The summary line stays visible when collapsed: load, memory and addresses
// are the part worth having on screen at all times.
func TestVMSummaryAlwaysVisible(t *testing.T) {
	m := &Model{VM: vmWithProcs(), Width: 120, Height: 40}
	out := Render(m, NewPlainRenderer())
	for _, want := range []string{"linux 7.1.8-arch1-3", "20 cpu", "mem "} {
		if !strings.Contains(out, want) {
			t.Fatalf("summary must stay visible, missing %q:\n%s", want, out)
		}
	}
}

// [V] expands the list, and again collapses it.
func TestVMProcsKeyToggles(t *testing.T) {
	m := &Model{VM: vmWithProcs(), Width: 120, Height: 40}

	if !m.Update(KeyVMProcs) {
		t.Fatal("V must toggle the process list")
	}
	out := Render(m, NewPlainRenderer())
	if !strings.Contains(out, "brave") || !strings.Contains(out, "[V] hide") {
		t.Fatalf("expanded panel must list processes:\n%s", out)
	}

	m.Update(KeyVMProcs)
	if strings.Contains(Render(m, NewPlainRenderer()), "brave") {
		t.Fatal("V again must collapse the list")
	}
}

// A host reporting no processes shows neither the hint nor an empty table.
func TestVMNoProcessesNoHint(t *testing.T) {
	m := &Model{VM: VMState{Kernel: "6.1", CPUs: 4}, Width: 120, Height: 40}
	out := Render(m, NewPlainRenderer())
	if strings.Contains(out, "[V] show") {
		t.Fatalf("no processes must not offer an expand hint:\n%s", out)
	}
}

// V must not fire while the logs panel owns the key stream: there, V is not a
// binding at all and the panel must keep its own behaviour.
func TestVMProcsDoesNotCollideWithVMLogs(t *testing.T) {
	m := &Model{VM: vmWithProcs()}
	m.Update(KeyVMLogs)
	if m.VMProcsOpen {
		t.Fatal("v (vm logs) must not expand the process list")
	}
}
