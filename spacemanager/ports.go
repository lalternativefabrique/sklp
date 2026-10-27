package spacemanager

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// PortMapping is one host->space ingress: traffic to BindHost:Host on the
// HOST is proxied to ContainerPort on a Ready replica of the service (round-robin
// across replicas). It is the parsed form of a docker-style "ports:" entry.
type PortMapping struct {
	BindHost      string // host address the proxy listens on; empty = loopback
	Host          int    // host TCP port the proxy listens on
	Container     int    // port inside the space the proxy dials on each replica
	ProxyProtocol bool   // prefix each relayed connection with a PROXY v2 header
	UDP           bool   // relay datagrams instead of TCP connections
}

// applyProxyProtocol flags the mappings whose container port is listed in
// ports. A listed port with no mapping is an error: the service would expect a
// PROXY header on a port nobody relays to.
func applyProxyProtocol(maps []PortMapping, ports []int) ([]PortMapping, error) {
	for _, p := range ports {
		found := false
		for i := range maps {
			if maps[i].Container == p && !maps[i].UDP {
				maps[i].ProxyProtocol = true
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("proxy_protocol: container port %d has no TCP ports: mapping", p)
		}
	}
	return maps, nil
}

const maxPortRange = 1000

// parsePortMapping parses one single-port docker-style entry. A range is
// rejected: callers that need exactly one mapping (ingress) use this.
func parsePortMapping(spec string) (PortMapping, error) {
	maps, err := parsePortSpec(spec)
	if err != nil {
		return PortMapping{}, err
	}
	if len(maps) != 1 {
		return PortMapping{}, fmt.Errorf("port mapping %q: a port range is not allowed here", spec)
	}
	return maps[0], nil
}

// parsePortSpec parses one docker-style ports entry, expanding a range into
// one PortMapping per port. Accepted forms:
//
//	"8080:80"                    host loopback 8080 -> container 80
//	"0.0.0.0:8080:80"            bind the given host address instead of loopback
//	"0.0.0.0:7882:7882/udp"      relay UDP datagrams; "/tcp" is the default
//	"0.0.0.0:50000-50100:50000-50100/udp"  equal-length ranges, at most 1000 ports
func parsePortSpec(spec string) ([]PortMapping, error) {
	s := strings.TrimSpace(spec)
	if s == "" {
		return nil, fmt.Errorf("empty port mapping")
	}
	udp := false
	if body, proto, ok := strings.Cut(s, "/"); ok {
		switch proto {
		case "tcp":
		case "udp":
			udp = true
		default:
			return nil, fmt.Errorf("port mapping %q: protocol %q is not supported (tcp or udp)", spec, proto)
		}
		s = body
	}

	parts := strings.Split(s, ":")
	var bindHost, hostStr, ctrStr string
	switch len(parts) {
	case 2:
		hostStr, ctrStr = parts[0], parts[1]
	case 3:
		bindHost = strings.TrimSpace(parts[0])
		if net.ParseIP(bindHost) == nil {
			return nil, fmt.Errorf("port mapping %q: bind address %q is not an IP", spec, bindHost)
		}
		hostStr, ctrStr = parts[1], parts[2]
	default:
		return nil, fmt.Errorf("port mapping %q: want \"HOST:CONTAINER\"", spec)
	}

	hostLo, hostHi, err := parsePortRange(hostStr)
	if err != nil {
		return nil, fmt.Errorf("port mapping %q: host port: %w", spec, err)
	}
	ctrLo, ctrHi, err := parsePortRange(ctrStr)
	if err != nil {
		return nil, fmt.Errorf("port mapping %q: container port: %w", spec, err)
	}
	if hostHi-hostLo != ctrHi-ctrLo {
		return nil, fmt.Errorf("port mapping %q: host range has %d ports, container range has %d", spec, hostHi-hostLo+1, ctrHi-ctrLo+1)
	}
	out := make([]PortMapping, 0, hostHi-hostLo+1)
	for i := 0; i <= hostHi-hostLo; i++ {
		out = append(out, PortMapping{BindHost: bindHost, Host: hostLo + i, Container: ctrLo + i, UDP: udp})
	}
	return out, nil
}

func parsePortRange(s string) (int, int, error) {
	loStr, hiStr, isRange := strings.Cut(s, "-")
	lo, err := parsePort(loStr)
	if err != nil {
		return 0, 0, err
	}
	if !isRange {
		return lo, lo, nil
	}
	hi, err := parsePort(hiStr)
	if err != nil {
		return 0, 0, err
	}
	if lo > hi {
		return 0, 0, fmt.Errorf("range %q is reversed", s)
	}
	if hi-lo+1 > maxPortRange {
		return 0, 0, fmt.Errorf("range %q spans %d ports (max %d)", s, hi-lo+1, maxPortRange)
	}
	return lo, hi, nil
}

// parsePort parses a 1..65535 TCP port.
func parsePort(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("missing")
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number", s)
	}
	if n < 1 || n > 65535 {
		return 0, fmt.Errorf("%d out of range (1..65535)", n)
	}
	return n, nil
}

// servicePortMappings is the parsed, proxy-protocol-flagged ingress set of svc.
func servicePortMappings(svc Service) ([]PortMapping, error) {
	maps, err := parsePortMappings(svc.Ports)
	if err != nil {
		return nil, err
	}
	return applyProxyProtocol(maps, svc.ProxyProtocol)
}

// parsePortMappings parses every entry of a service's ports list, failing on
// the first malformed one so a bad spec is surfaced at deploy time, not silently
// dropped. An empty list yields nil.
func parsePortMappings(specs []string) ([]PortMapping, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	out := make([]PortMapping, 0, len(specs))
	for _, s := range specs {
		maps, err := parsePortSpec(s)
		if err != nil {
			return nil, err
		}
		out = append(out, maps...)
	}
	return out, nil
}
