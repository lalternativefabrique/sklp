//go:build linux

package spacenet

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/lalternativefabrique/sklp/ingress"
	"github.com/vishvananda/netns"
)

// l7_linux.go runs sklp's NATIVE L7 ingress as a goroutine of the space worker,
// instead of a separate traefik container. The worker already lives in the space
// netns and IS the DNS table, so it reverse-proxies a request straight to a
// service by name — no container, no image, no bind-mounted binary. The host mux
// fd-passes HTTPS/path traffic to this listener; plain Host routes still go
// direct to the service (the mux dials them itself).
//
// The L7 listener binds the space gateway IP on a fixed port inside the netns, so
// the mux's worker-side dial (service "sklp-l7", port l7Port) reaches it like any
// other backend.

// l7Port / l7TLSPort are the in-netns ports the native L7 ingress listens on:
// HTTP (the mux's Host/path routes) and HTTPS (the mux's SNI routes, terminated
// here). Distinct from any user service port; unprivileged.
const (
	l7Port    = 8001
	l7TLSPort = 8443
)

// l7ServiceName / l7TLSServiceName are the reserved DNS names the worker
// registers its own HTTP and TLS L7 endpoints under, so the mux's
// dialReplicaInNetns resolves them like a service.
const (
	l7ServiceName    = "sklp-l7"
	l7TLSServiceName = "sklp-l7-tls"
)

// l7Ingress bundles the running L7 handler with its optional ACME resolver, so a
// route-table swap (applyL7Routes) can both hot-reload routes AND reconcile the
// set of public domains under ACME management. A nil l7Ingress (start failed)
// disables L7 entirely; a nil acme means self-signed only.
type l7Ingress struct {
	handler *ingress.Handler
	acme    *ingress.ACMEResolver // nil when ACME is not configured
}

// startL7Ingress starts the native L7 reverse proxy in the worker, listening on
// the gateway IP inside the space netns. It returns the handler + optional ACME
// resolver (so l7routes commands hot-swap the table and reconcile managed
// domains) or nil on failure. Best-effort: a listen failure only disables L7
// routing; direct Host routing via the mux is unaffected. names lets the worker
// register the L7 endpoint in its own DNS. TLS termination uses an ACME cert for
// managed public domains (when SKLP_SPACE_ACME is set) and per-SNI self-signed
// for everything else.
func startL7Ingress(capNS netns.NsHandle, names *nameTable, gwIP net.IP) *l7Ingress {
	// Resolve a backend's service name via the in-memory nameTable (the space
	// DNS), then dial its IP — Go's default resolver would read the HOST
	// /etc/resolv.conf, which never sees the space DNS, so a name backend would
	// 502. Splits host:port, looks the name up (round-robin), dials the first IP.
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		var d net.Dialer
		// Already an IP? dial directly.
		if net.ParseIP(host) != nil {
			return d.DialContext(ctx, network, addr)
		}
		ips, ok := names.lookup(host)
		if !ok || len(ips) == 0 {
			return nil, fmt.Errorf("l7: no ready replica for %q", host)
		}
		return d.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
	}
	h := ingress.New().WithDialer(dial)

	// Listen on the gateway IP inside the netns so the proxy is reachable from the
	// worker's own dial path (the mux asks for service l7ServiceName:l7Port). The
	// listener is created ON the netns-pinned worker thread, so it binds inside
	// the space netns without an explicit setns.
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", gwIP.String(), l7Port))
	if err != nil {
		fmt.Fprintf(os.Stderr, "spacenet worker: L7 ingress listen: %v\n", err)
		return nil
	}
	// Register the L7 endpoint under its reserved name so the mux's nameTable
	// lookup (dialReplicaInNetns) resolves it to the gateway IP.
	names.register(l7ServiceName, gwIP)

	srv := newL7Server(h)
	go func() {
		if serr := srv.Serve(ln); serr != nil && serr != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "spacenet worker: L7 ingress serve: %v\n", serr)
		}
	}()

	// The relayed listener: reached only through the node's Network
	// Controller, which registers it under ingress.GatewayService, so the
	// X-Forwarded-* headers a front sends are trusted there (ADR-021).
	if relayLn, rerr := net.Listen("tcp", fmt.Sprintf("%s:%d", gwIP.String(), ingress.GatewayPort)); rerr != nil {
		fmt.Fprintf(os.Stderr, "spacenet worker: L7 relayed listen: %v\n", rerr)
	} else {
		names.register(ingress.GatewayService, gwIP)
		relaySrv := newL7Server(ingress.Relayed(h))
		go func() {
			if serr := relaySrv.Serve(relayLn); serr != nil && serr != http.ErrServerClosed {
				fmt.Fprintf(os.Stderr, "spacenet worker: L7 relayed serve: %v\n", serr)
			}
		}()
	}

	// Pick the TLS cert source: an ACME resolver (real Let's Encrypt certs via
	// dns-01 for managed public domains, self-signed fallback for the rest) when
	// SKLP_SPACE_ACME is set, else per-SNI self-signed only. A malformed/invalid
	// ACME env degrades to self-signed rather than failing the whole L7 — HTTPS
	// still works (curl -k), and the operator sees the error on stderr.
	acmeR, getCert := l7CertSource(gwIP)

	// HTTPS endpoint: terminate TLS here. The mux SNI-routes HTTPS to
	// l7TLSServiceName:l7TLSPort; this listener terminates and proxies to the
	// backend over plain HTTP, same handler.
	if tlsLn, terr := net.Listen("tcp", fmt.Sprintf("%s:%d", gwIP.String(), l7TLSPort)); terr != nil {
		fmt.Fprintf(os.Stderr, "spacenet worker: L7 TLS listen: %v\n", terr)
	} else {
		names.register(l7TLSServiceName, gwIP)
		tlsSrv := newL7Server(h)
		tlsSrv.TLSConfig = &tls.Config{GetCertificate: getCert}
		go func() {
			if serr := tlsSrv.ServeTLS(tlsLn, "", ""); serr != nil && serr != http.ErrServerClosed {
				fmt.Fprintf(os.Stderr, "spacenet worker: L7 TLS serve: %v\n", serr)
			}
		}()
	}
	return &l7Ingress{handler: h, acme: acmeR}
}

// L7 server deadlines. The mux's peekTimeout only bounds the PEEK: once the
// Host/SNI is parsed the fd is spliced through and the mux leaves the path, so
// without these the L7 holds a silent connection forever. The rate limiter does
// not cover it either — it runs in ServeHTTP, i.e. after the headers are fully
// read, so a client that sends just enough to parse and then stops never
// reaches it.
const (
	// l7ReadHeaderTimeout is the slowloris cut: a client has this long to
	// finish sending its request headers (and, on the TLS listener, to complete
	// the handshake) before the connection is dropped.
	l7ReadHeaderTimeout = 10 * time.Second

	// l7IdleTimeout bounds a kept-alive connection between requests, so an
	// established-then-silent connection does not hold its fds indefinitely.
	l7IdleTimeout = 120 * time.Second

	// l7MaxHeaderBytes caps the request head. Go's own default is 1 MiB, which
	// is a lot of memory to grant an unauthenticated client per connection.
	l7MaxHeaderBytes = 64 << 10
)

// newL7Server builds the L7 http.Server with the deadlines above.
//
// ReadTimeout and WriteTimeout are deliberately NOT set: they are absolute
// caps on a whole request and a whole response, so any value would also cut a
// legitimate large upload, a long download on a slow link, and every WebSocket
// or SSE stream (httputil.ReverseProxy proxies upgrades, so a space's app may
// well serve them). ReadHeaderTimeout closes the slowloris without that cost —
// the same trade traefik makes with its own writeTimeout defaulting to 0.
// A slow-READ client is therefore still out of scope here.
func newL7Server(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: l7ReadHeaderTimeout,
		IdleTimeout:       l7IdleTimeout,
		MaxHeaderBytes:    l7MaxHeaderBytes,
	}
}

// l7CertSource builds the TLS GetCertificate callback for the L7 listener from
// SKLP_SPACE_ACME. When ACME is configured and the resolver builds cleanly it
// returns the ACME resolver (managed public domains -> Let's Encrypt, rest ->
// self-signed) and its GetCertificate. Otherwise it returns a nil resolver and a
// plain per-SNI self-signed source, so TLS always terminates.
func l7CertSource(gwIP net.IP) (*ingress.ACMEResolver, func(*tls.ClientHelloInfo) (*tls.Certificate, error)) {
	cfg, ok, err := ingress.UnmarshalACMEEnv(os.Getenv(ingress.EnvACME))
	if err != nil {
		fmt.Fprintf(os.Stderr, "spacenet worker: ACME config ignored (%v); serving self-signed\n", err)
	}
	if ok {
		// Point certmagic at the space DNS (gwIP:53) so it can resolve the ACME CA
		// and the DNS-provider API from inside the netns — the host resolver in
		// /etc/resolv.conf is unreachable here.
		if gwIP != nil {
			cfg.SpaceDNSAddr = net.JoinHostPort(gwIP.String(), dnsPort)
		}
		if r, rerr := ingress.NewACMEResolver(cfg); rerr != nil {
			fmt.Fprintf(os.Stderr, "spacenet worker: ACME disabled (%v); serving self-signed\n", rerr)
		} else {
			return r, r.GetCertificate
		}
	}
	return nil, ingress.NewSelfSignedResolver().GetCertificate
}

// gatewayIPFromCIDR extracts the gateway IP from a CIDR like "10.88.0.1/24".
// Returns nil on a parse error (startL7Ingress then fails its listen and L7 is
// simply disabled — east-west networking is untouched).
func gatewayIPFromCIDR(cidr string) net.IP {
	ip, _, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil
	}
	return ip
}

// applyL7Routes decodes the JSON route table from an l7routes command, swaps it
// into the running handler, and — when ACME is active — reconciles the set of
// public domains under management from the route hosts (so a newly-routed public
// host triggers async issuance and a removed one stops renewing). A nil l7 (L7
// never started) is a no-op.
func applyL7Routes(l7 *l7Ingress, routesJSON string) error {
	if l7 == nil || l7.handler == nil {
		return nil
	}
	var routes []ingress.Route
	if routesJSON != "" {
		if err := json.Unmarshal([]byte(routesJSON), &routes); err != nil {
			return fmt.Errorf("decode l7 routes: %w", err)
		}
	}
	l7.handler.SetRoutes(routes)

	if l7.acme != nil {
		// Manage only public hosts — dev/bare hosts (*.test, localhost, ...) can
		// never pass an ACME challenge and must stay on self-signed. Dedup because
		// several routes (distinct paths) can share one host.
		seen := map[string]bool{}
		var hosts []string
		for _, r := range routes {
			if r.Host == "" || seen[r.Host] || !ingress.IsPublicDomain(r.Host) {
				continue
			}
			seen[r.Host] = true
			hosts = append(hosts, r.Host)
		}
		if err := l7.acme.ManageDomains(context.Background(), hosts); err != nil {
			// Non-fatal: routes are already live; issuance can catch up. Surface it.
			fmt.Fprintf(os.Stderr, "spacenet worker: ACME manage domains: %v\n", err)
		}
	}
	return nil
}
