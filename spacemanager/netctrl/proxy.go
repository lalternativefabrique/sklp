package netctrl

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
)

// httpProxy routes by Host header: "service1.spaceB.internal" -> the real
// backend Registry.Lookup("spaceB", "service1") resolves to. A Host that
// doesn't parse as <service>.<space>.internal, or isn't registered, gets a
// plain 502 — there is nothing else this proxy is meant to serve.
type httpProxy struct {
	reg    *Registry
	server *http.Server
}

// newHTTPProxy binds addrs (one or more, e.g. the controller's local IP and,
// when mesh is joined, its WireGuard mesh IP too) to the SAME handler and
// routing table — a remote node's caller lands on the exact same
// Director/ErrorHandler a local caller would, just reached over a different
// interface. At least one address is required.
func newHTTPProxy(reg *Registry, addrs ...string) (*httpProxy, error) {
	p := &httpProxy{reg: reg}
	rp := &httputil.ReverseProxy{
		Director: p.direct,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "netctrl: no such service", http.StatusBadGateway)
		},
	}
	p.server = &http.Server{Handler: rp}

	var lns []net.Listener
	for _, addr := range addrs {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			for _, opened := range lns {
				opened.Close()
			}
			return nil, fmt.Errorf("netctrl: http proxy listen %s: %w", addr, err)
		}
		lns = append(lns, ln)
	}
	for _, ln := range lns {
		go p.server.Serve(ln)
	}
	return p, nil
}

func (p *httpProxy) direct(req *http.Request) {
	service, space, ok := parseInternalName(hostOnly(req.Host) + ".")
	if !ok {
		req.URL = &url.URL{} // Director errors are swallowed by ReverseProxy; an empty URL forces a dial failure caught by ErrorHandler
		return
	}
	ep, ok := p.reg.Lookup(space, service)
	if !ok {
		req.URL = &url.URL{}
		return
	}
	req.URL.Scheme = "http"
	req.URL.Host = net.JoinHostPort(ep.IP, strconv.Itoa(ep.Port))
}

func (p *httpProxy) close() error {
	return p.server.Close()
}

// hostOnly strips a ":port" suffix some clients include in the Host header.
// SplitHostPort errors when there is no port at all, which is the common
// case here — the bare host is already correct, so that error is expected
// and ignored rather than a real failure.
func hostOnly(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}
