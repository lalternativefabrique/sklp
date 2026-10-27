package mux

import (
	"context"
	"fmt"
	"io"
	"time"
)

// refreshInterval is how often the mux re-scans the host for each space's
// ingress. Short enough that a freshly deployed space is routed within seconds,
// long enough to stay a cheap directory read.
const refreshInterval = 3 * time.Second

// Run is the host front door in ingress-per-space mode: it binds listen (HTTP,
// routed by Host header) and listenTLS (HTTPS, routed by SNI, passthrough),
// re-scans the live spaces every few seconds, and serves until ctx is cancelled.
//
// It lives in the mux package rather than in a CLI so BOTH binaries can run it.
// The unit that starts the mux is written with whichever binary is on the node —
// sklet on a deploy node — and a verb only sklp had made that unit exit
// 2/INVALIDARGUMENT on every restart, leaving the node with no front door and
// nothing in the deploy log to say so.
func Run(ctx context.Context, listen, listenTLS string, out, errOut io.Writer) error {
	m := New()
	if err := m.Listen(listen); err != nil {
		return err
	}
	if listenTLS != "" {
		if err := m.ListenTLS(listenTLS); err != nil {
			return err
		}
	}
	defer m.Close()

	refresh := func() {
		table, err := HostTargets()
		if err != nil {
			fmt.Fprintln(errOut, "gateway mux: route scan:", err)
			return
		}
		m.SetRoutes(table)
	}
	refresh()

	if listenTLS != "" {
		fmt.Fprintf(out, "gateway mux: listening on %s (HTTP) and %s (HTTPS/SNI)\n", listen, listenTLS)
	} else {
		fmt.Fprintf(out, "gateway mux: listening on %s\n", listen)
	}

	go func() {
		ticker := time.NewTicker(refreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refresh()
			}
		}
	}()

	serveErr := make(chan error, 1)
	go func() { serveErr <- m.Serve() }()
	select {
	case <-ctx.Done():
		return nil
	case err := <-serveErr:
		return err
	}
}
