package pipeline

import (
	"net/url"
	"strconv"
	"strings"
)

// ComposeReadyHTTP resolves a service's HTTP readiness target into a dialable
// URL, given the service's declared container port.
//
// The contract is port-first: `port:` is the source of truth, and `ready.http`
// is normally a bare path (`/health`) composed onto `http://localhost:<port>`.
// Rules, in order:
//
//   - empty http             -> "" (no HTTP probe)
//   - path form ("/health")  -> "http://localhost:<port>/health" (port required)
//   - bare path, port == 0   -> "" (cannot compose; caller treats as no probe)
//   - absolute URL           -> returned verbatim (backward compatibility)
//
// It is pure (no I/O) so the contract is unit-testable without a probe dial.
func ComposeReadyHTTP(httpField string, port int) string {
	if httpField == "" {
		return ""
	}
	// A bare path is composed onto the declared port. A full scheme://host
	// URL is returned as-is.
	if strings.HasPrefix(httpField, "/") {
		if port <= 0 {
			return ""
		}
		return "http://localhost:" + strconv.Itoa(port) + httpField
	}
	return httpField
}

// PortFromReadyHTTP extracts the container port a service listens on, port-first:
// an explicit `port:` wins; otherwise the port is parsed from an absolute
// `ready.http` URL (backward compatibility). Returns ok=false when neither
// yields a port (no `port:` and a path/TCP/log probe) — such a service is
// reachable east-west by DNS only, not host-exposed.
func PortFromReadyHTTP(port int, httpField string) (int, bool) {
	if port > 0 {
		return port, true
	}
	if httpField == "" || strings.HasPrefix(httpField, "/") {
		return 0, false
	}
	u, err := url.Parse(httpField)
	if err != nil || u.Host == "" {
		return 0, false
	}
	p := u.Port()
	if p == "" {
		return 0, false
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		return 0, false
	}
	return n, true
}
