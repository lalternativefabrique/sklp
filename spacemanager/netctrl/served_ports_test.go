package netctrl

import (
	"net"
	"sort"
	"testing"
)

func TestOnServedPorts_FollowsRegisterAndUnregister(t *testing.T) {
	r := newTCPRouter(net.IPv4(127, 0, 0, 1))
	defer r.close()
	c := &Controller{tcp: r}
	var got []int
	c.OnServedPorts(53, 80, func(p []int) { got = append([]int(nil), p...); sort.Ints(got) })
	if len(got) != 2 || got[0] != 53 || got[1] != 80 {
		t.Fatalf("initial served ports = %v, want [53 80]", got)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	if err := r.Register("s", "db", port, Endpoint{IP: "127.0.0.1", Port: 1}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || sort.SearchInts(got, port) == len(got) || got[sort.SearchInts(got, port)] != port {
		t.Fatalf("after register: %v, want 53, 80 and %d", got, port)
	}
	r.Unregister("s", "db")
	if len(got) != 2 {
		t.Fatalf("after unregister: %v, want [53 80]", got)
	}
}
