package control

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/lalternativefabrique/sklp/spacemanager"
)

const dialConnectTimeout = 5 * time.Second

func dialEndpoint(sup Supervisor, svc string) (string, error) {
	if !sup.HasService(svc) {
		return "", fmt.Errorf("unknown service %s", svc)
	}
	for _, s := range sup.Snapshot() {
		if s.Name != svc {
			continue
		}
		for _, inst := range s.Instances {
			if (inst.State == spacemanager.Ready || inst.State == spacemanager.Running) && inst.IP != "" && inst.Port > 0 {
				if addr, err := sup.HostBackend(svc); err == nil && addr != "" {
					return addr, nil
				}
				return net.JoinHostPort(inst.IP, fmt.Sprint(inst.Port)), nil
			}
		}
		return "", fmt.Errorf("%s has no live instance with a declared port", svc)
	}
	return "", fmt.Errorf("unknown service %s", svc)
}

func handleDial(ctx context.Context, w http.ResponseWriter, r *http.Request, space string, sup Supervisor) {
	svc := strings.TrimPrefix(r.URL.Path, RouteDial)
	if err := validServiceName(svc); err != nil {
		http.Error(w, "dial: "+err.Error(), http.StatusBadRequest)
		return
	}
	addr, err := dialEndpoint(sup, svc)
	if err != nil {
		http.Error(w, "dial: "+err.Error(), http.StatusNotFound)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "dial: server does not support connection hijacking", http.StatusInternalServerError)
		return
	}
	upstream, err := (&net.Dialer{Timeout: dialConnectTimeout}).DialContext(ctx, "tcp", addr)
	if err != nil {
		http.Error(w, "dial: "+svc+" refused the connection: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer upstream.Close()

	conn, buffered, err := hj.Hijack()
	if err != nil {
		http.Error(w, "dial: hijack failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nConnection: upgrade\r\nUpgrade: sklp-dial\r\n\r\n")); err != nil {
		return
	}
	log.Printf("dial: %s/%s opened from %s", space, svc, r.RemoteAddr)
	started := time.Now()
	in, out := relay(ctx, conn, buffered.Reader, upstream)
	log.Printf("dial: %s/%s closed after %s (%d B in, %d B out)", space, svc, time.Since(started).Round(time.Millisecond), in, out)
}

// relay copies both ways until either side closes or ctx ends; bytes the
// HTTP reader had already buffered belong to the stream and go first.
func relay(ctx context.Context, client net.Conn, clientBuffered io.Reader, upstream net.Conn) (in, out int64) {
	var toUpstream, toClient atomic.Int64
	done := make(chan struct{}, 2)
	go func() {
		n, _ := io.Copy(upstream, clientBuffered)
		toUpstream.Store(n)
		if cw, ok := upstream.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		n, _ := io.Copy(client, upstream)
		toClient.Store(n)
		if cw, ok := client.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
	return toUpstream.Load(), toClient.Load()
}
