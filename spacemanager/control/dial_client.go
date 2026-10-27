package control

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
)

// DialService returns a TCP stream to a live instance of svc on its declared
// port, relayed by the node. The caller speaks the service's own protocol on
// it; closing it ends the relay.
func (c *Client) DialService(ctx context.Context, svc string) (net.Conn, error) {
	conn, err := c.dialRaw(ctx)
	if err != nil {
		return nil, c.dialErr(err)
	}
	u := c.route(RouteDial) + url.PathEscape(svc)
	reqLine := fmt.Sprintf("POST %s HTTP/1.1\r\nHost: %s\r\n%sContent-Length: 0\r\nConnection: upgrade\r\n\r\n", u, c.rawHost, c.rawAuth)
	if _, err := conn.Write([]byte(reqLine)); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodPost})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read dial response: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		conn.Close()
		return nil, fmt.Errorf("control: dial %q in space %q: status %d: %s", svc, c.space, resp.StatusCode, bytes.TrimSpace(msg))
	}
	return &bufferedConn{Conn: conn, r: br}, nil
}

// bufferedConn hands back the bytes the HTTP reader consumed past the 101
// before the raw stream is read; without it the first reply could be lost.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }
