package control

import (
	"bufio"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestControlServerClosesIdleKeepAlive(t *testing.T) {
	prev := controlIdleTimeout
	controlIdleTimeout = 200 * time.Millisecond
	defer func() { controlIdleTimeout = prev }()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := newControlServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("idle keep-alive connection still open after the idle timeout")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("server never closed the idle keep-alive connection")
	}
}
