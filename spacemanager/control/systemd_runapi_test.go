package control

import (
	"fmt"
	"strings"
	"testing"
)

func TestRenderRunAPIUnitOmitsEmptyFlags(t *testing.T) {
	unit := renderRunAPIUnit("/usr/local/bin/sklp", RunAPIOptions{Addr: ":6464", Concurrency: 1})
	if !strings.Contains(unit, "ExecStart=/usr/local/bin/sklp run --listen :6464\n") {
		t.Fatalf("unexpected ExecStart:\n%s", unit)
	}
	if strings.Contains(unit, "--advertise") || strings.Contains(unit, "--work-root") || strings.Contains(unit, "--concurrency") {
		t.Fatalf("empty/default flags must be omitted, not passed blank:\n%s", unit)
	}
}

func TestRenderRunAPIUnitIncludesSetFlags(t *testing.T) {
	unit := renderRunAPIUnit("/usr/local/bin/sklp", RunAPIOptions{Addr: ":6464", Advertise: "192.168.100.45", WorkRoot: "/var/cache/sklp", Concurrency: 3})
	want := "ExecStart=/usr/local/bin/sklp run --listen :6464 --advertise 192.168.100.45 --work-root /var/cache/sklp --concurrency 3\n"
	if !strings.Contains(unit, want) {
		t.Fatalf("ExecStart missing flags:\n%s", unit)
	}
}

// Restart=always, not on-failure: a run API that exits cleanly still leaves the
// node unable to accept work.
func TestRenderRunAPIUnitRestartsAlways(t *testing.T) {
	unit := renderRunAPIUnit("/usr/local/bin/sklp", RunAPIOptions{Addr: ":6464", Concurrency: 1})
	if !strings.Contains(unit, "Restart=always") {
		t.Fatalf("expected Restart=always:\n%s", unit)
	}
	if !strings.Contains(unit, "WantedBy=default.target") {
		t.Fatalf("unit must be enabled at boot:\n%s", unit)
	}
}

func TestRunAPIUnitNameIsFixed(t *testing.T) {
	if RunAPIUnitName != "sklp-run-api.service" {
		t.Fatalf("unit name is part of the operator interface: %q", RunAPIUnitName)
	}
	if !strings.HasSuffix(RunAPIUnitPath(), "systemd/user/"+RunAPIUnitName) {
		t.Fatalf("unit path = %q", RunAPIUnitPath())
	}
}

// Both long-lived network units must raise the descriptor ceiling. The usual
// 1024 default caps the mux — the front door of every space, holding two
// descriptors per connection in flight — around 500 concurrent connections,
// which ordinary traffic across a few busy sites reaches on its own.
func TestNetworkUnitsRaiseNOFILE(t *testing.T) {
	want := fmt.Sprintf("LimitNOFILE=%d", systemdNOFILE)
	if systemdNOFILE < 65536 {
		t.Errorf("systemdNOFILE = %d; the point is to leave the 1024 default far behind", systemdNOFILE)
	}
	for name, unit := range map[string]string{
		"mux":    renderMuxUnit("/usr/local/bin/sklp", ":9080"),
		"runapi": renderRunAPIUnit("/usr/local/bin/sklp", RunAPIOptions{Addr: ":9000"}),
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("%s unit missing %q:\n%s", name, want, unit)
		}
	}
}

func TestRunAPIUnitPersistsAllowHostSteps(t *testing.T) {
	if strings.Contains(renderRunAPIUnit("/usr/local/bin/sklp", RunAPIOptions{Addr: ":6464"}), "--allow-host-steps") {
		t.Fatal("host steps enabled without being asked for")
	}
	if !strings.Contains(renderRunAPIUnit("/usr/local/bin/sklp", RunAPIOptions{Addr: ":6464", AllowHostSteps: true}), " --allow-host-steps") {
		t.Fatal("--allow-host-steps not persisted into ExecStart")
	}
}
