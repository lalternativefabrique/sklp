package spacenet

import (
	"fmt"
	"sort"
	"strings"
)

// hostGuardTable is the nft table, in each space netns, that keeps a
// container off its own node. slirp4netns opens every outbound connection
// from the host itself, so any address the node owns — its private IP, its
// public IP — leads to whatever listens there: sshd, the sklet control API.
// The RFC1918 blackhole does not cover a public address, and the /32 hole
// punched for the network controller opens the private one on every port.
// Only the controller's own services stay reachable, on the ports it serves.
const hostGuardTable = "sklp_hostguard"

// hostGuardRules renders the guard as one atomic nft script. hostAddrs are
// every IPv4 address the node owns; controllerAddrs are the addresses the
// network controller listens on (its private IP, and this node's own mesh IP
// when joined); ports are the TCP ports it serves there. DNS over UDP is
// always allowed to the controller. A drop is final in nftables, so the
// egress allowlist still applies to whatever this lets through.
func hostGuardRules(tap string, hostAddrs, controllerAddrs []string, ports []int) string {
	return hostGuardRulesWithPublic(tap, hostAddrs, controllerAddrs, ports, nil)
}

// publicGatewayPorts are the only ports a container may dial on the node's
// public address: the gateway, which the Internet reaches there anyway, so a
// service calling a site hosted on its own node by its public name still
// works. Everything else on that address — sshd, the control API — is not.
var publicGatewayPorts = []int{80, 443}

// hostGuardRulesWithPublic adds the node's public addresses, which are not on
// any interface when the provider NATs a floating IP: slirp dials them, the
// provider's router sends the connection straight back to the node.
func hostGuardRulesWithPublic(tap string, hostAddrs, controllerAddrs []string, ports []int, publicAddrs []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "add table ip %s\n", hostGuardTable)
	fmt.Fprintf(&b, "flush table ip %s\n", hostGuardTable)
	fmt.Fprintf(&b, "add chain ip %s forward { type filter hook forward priority -10 ; policy accept ; }\n", hostGuardTable)
	if len(hostAddrs) == 0 && len(publicAddrs) == 0 {
		return b.String()
	}
	fmt.Fprintf(&b, "add rule ip %s forward ct state established,related accept\n", hostGuardTable)
	ctrl := setLiteral(controllerAddrs)
	if ctrl != "" {
		fmt.Fprintf(&b, "add rule ip %s forward oifname \"%s\" ip daddr %s udp dport 53 accept\n", hostGuardTable, tap, ctrl)
		if p := portLiteral(ports); p != "" {
			fmt.Fprintf(&b, "add rule ip %s forward oifname \"%s\" ip daddr %s tcp dport %s accept\n", hostGuardTable, tap, ctrl, p)
		}
	}
	if pub := setLiteral(publicAddrs); pub != "" {
		fmt.Fprintf(&b, "add rule ip %s forward oifname \"%s\" ip daddr %s tcp dport %s accept\n", hostGuardTable, tap, pub, portLiteral(publicGatewayPorts))
		fmt.Fprintf(&b, "add rule ip %s forward oifname \"%s\" ip daddr %s drop\n", hostGuardTable, tap, pub)
	}
	fmt.Fprintf(&b, "add rule ip %s forward oifname \"%s\" ip daddr %s drop\n", hostGuardTable, tap, setLiteral(hostAddrs))
	return b.String()
}

func setLiteral(addrs []string) string {
	var out []string
	for _, a := range addrs {
		a = strings.TrimSuffix(strings.TrimSpace(a), "/32")
		if a != "" {
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		return ""
	}
	return "{ " + strings.Join(out, ", ") + " }"
}

func portLiteral(ports []int) string {
	seen := map[int]bool{}
	var ps []int
	for _, p := range ports {
		if p > 0 && p <= 65535 && !seen[p] {
			seen[p] = true
			ps = append(ps, p)
		}
	}
	if len(ps) == 0 {
		return ""
	}
	sort.Ints(ps)
	s := make([]string, len(ps))
	for i, p := range ps {
		s[i] = fmt.Sprint(p)
	}
	return "{ " + strings.Join(s, ", ") + " }"
}
