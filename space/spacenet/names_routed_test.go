//go:build linux

package spacenet

import (
	"net"
	"testing"
)

func ipStrings(ips []net.IP) map[string]bool {
	out := map[string]bool{}
	for _, ip := range ips {
		out[ip.String()] = true
	}
	return out
}

func TestNameTable_RoutedSetHidesUnroutedReplicas(t *testing.T) {
	tbl := newNameTable()
	blue, green := net.ParseIP("10.0.0.2"), net.ParseIP("10.0.0.3")
	tbl.register("front", blue)
	tbl.register("front", green)

	tbl.route("front", []net.IP{blue})
	for i := 0; i < 4; i++ {
		ips, ok := tbl.lookup("front")
		if !ok || len(ips) != 1 || !ips[0].Equal(blue) {
			t.Fatalf("before cutover lookup #%d = %v ok=%v, want only blue", i, ips, ok)
		}
	}

	tbl.route("front", []net.IP{green})
	for i := 0; i < 4; i++ {
		ips, ok := tbl.lookup("front")
		if !ok || len(ips) != 1 || !ips[0].Equal(green) {
			t.Fatalf("after cutover lookup #%d = %v ok=%v, want only green", i, ips, ok)
		}
	}

	if got := ipStrings(tbl.members("FRONT")); !got[blue.String()] || !got[green.String()] {
		t.Fatalf("members = %v, want both replicas still attached", got)
	}

	tbl.route("front", []net.IP{blue})
	if ips, _ := tbl.lookup("front"); len(ips) != 1 || !ips[0].Equal(blue) {
		t.Fatalf("after rollback lookup = %v, want only blue", ips)
	}
}

func TestNameTable_PinnedEmptyResolvesToNothing(t *testing.T) {
	tbl := newNameTable()
	starting := net.ParseIP("10.0.0.5")
	tbl.register("api", starting)
	tbl.route("api", nil)

	ips, ok := tbl.lookup("api")
	if !ok || len(ips) != 0 {
		t.Fatalf("lookup = %v ok=%v, want a known name with no IPs", ips, ok)
	}

	tbl.route("api", []net.IP{starting})
	if ips, _ := tbl.lookup("api"); len(ips) != 1 {
		t.Fatalf("after Ready lookup = %v, want the instance", ips)
	}
}

func TestNameTable_RoutedIPNotRegisteredIsNeverServed(t *testing.T) {
	tbl := newNameTable()
	live, gone := net.ParseIP("10.0.0.2"), net.ParseIP("10.0.0.9")
	tbl.register("db", live)
	tbl.route("db", []net.IP{live, gone})

	ips, _ := tbl.lookup("db")
	if len(ips) != 1 || !ips[0].Equal(live) {
		t.Fatalf("lookup = %v, want only the registered IP", ips)
	}
}

func TestDispatch_RouteOpPinsTheNameTable(t *testing.T) {
	tbl := newNameTable()
	blue, green := net.ParseIP("10.0.0.2"), net.ParseIP("10.0.0.3")
	tbl.register("front", blue)
	tbl.register("front", green)

	if rep := dispatch(0, tbl, nil, command{Op: "route", Name: "front", IPs: []string{"10.0.0.3"}}); !rep.OK {
		t.Fatalf("route op failed: %s", rep.Error)
	}
	if ips, _ := tbl.lookup("front"); len(ips) != 1 || !ips[0].Equal(green) {
		t.Fatalf("lookup = %v, want only green", ips)
	}

	if rep := dispatch(0, tbl, nil, command{Op: "route", Name: "front", IPs: []string{"not-an-ip"}}); rep.OK {
		t.Fatal("route op must reject an invalid IP")
	}
	if ips, _ := tbl.lookup("front"); len(ips) != 1 || !ips[0].Equal(green) {
		t.Fatalf("a rejected route must leave the pin unchanged, lookup = %v", ips)
	}

}
