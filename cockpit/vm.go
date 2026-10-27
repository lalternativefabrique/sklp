package cockpit

import "time"

// VMState is what the guest can tell about the machine it runs on. The
// cockpit is relayed into a VM on macOS, so this is the only view its user
// gets of the box actually doing the work — everything else in the model
// describes what runs *inside* it.
//
// Every field is best-effort: a value the guest could not read is left zero
// and simply not rendered, so a missing /proc entry degrades the panel rather
// than failing the refresh.
type VMState struct {
	Backend   string // "vz", "lima", … — empty when it cannot be told
	Kernel    string
	CPUs      int
	LoadAvg   [3]float64
	MemTotal  uint64 // bytes
	MemAvail  uint64 // bytes
	Uptime    time.Duration
	Addrs     []string // non-loopback addresses, "iface 10.0.2.15/24"
	Processes []VMProcess
}

// VMProcess is one process worth showing: the workload, not the plumbing.
type VMProcess struct {
	PID  int
	Name string
	RSS  uint64 // bytes
}

// MemUsed reports resident memory in bytes, or 0 when either figure is
// missing (rather than an absurd number from subtracting a zero).
func (v VMState) MemUsed() uint64 {
	if v.MemTotal == 0 || v.MemAvail == 0 || v.MemAvail > v.MemTotal {
		return 0
	}
	return v.MemTotal - v.MemAvail
}

// Known reports whether anything at all could be read, so callers can skip
// the panel entirely on a host where none of this is available.
func (v VMState) Known() bool {
	return v.Kernel != "" || v.CPUs > 0 || v.MemTotal > 0
}
