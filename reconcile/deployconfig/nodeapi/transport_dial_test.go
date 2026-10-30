package nodeapi

import (
	"context"
	"errors"
	"net"
	"testing"
)

func TestClientForUsesDialContext(t *testing.T) {
	errDialed := errors.New("dialed through hook")
	var got string
	DialContext = func(_ context.Context, _, addr string) (net.Conn, error) {
		got = addr
		return nil, errDialed
	}
	t.Cleanup(func() { DialContext = nil; transports.Delete("hooktest") })

	_, err := clientFor("hooktest").Get("https://abc.nodes.example.org:6464/v1/health")
	if !errors.Is(err, errDialed) {
		t.Fatalf("err = %v, want the hook's error", err)
	}
	if got != "abc.nodes.example.org:6464" {
		t.Fatalf("hook dialed %q", got)
	}
}
