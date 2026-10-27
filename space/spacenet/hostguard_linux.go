//go:build linux

package spacenet

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// EnvNodePublicAddrs lists, comma-separated, the node's public IPv4 addresses
// that no interface carries (a provider's floating IP). Set by the agent from
// its --advertise address when that is an IP.
const EnvNodePublicAddrs = "SKLP_NODE_PUBLIC_ADDRS"

func nodePublicAddrs() []string {
	var out []string
	for _, a := range strings.Split(os.Getenv(EnvNodePublicAddrs), ",") {
		if ip := net.ParseIP(strings.TrimSpace(a)); ip != nil && ip.To4() != nil {
			out = append(out, ip.To4().String())
		}
	}
	return out
}

func programHostGuard(tap string, hostAddrs, controllerAddrs []string, ports []int, publicAddrs []string) error {
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(hostGuardRulesWithPublic(tap, hostAddrs, controllerAddrs, ports, publicAddrs))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nft host guard: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// hostOwnAddrs is every IPv4 address configured on the host's interfaces,
// bridges included, loopback excluded: all of them lead back to the node when
// slirp dials them. Resolved on the host stack, where the interfaces are.
func hostOwnAddrs() ([]string, error) {
	addrs, err := netlink.AddrList(nil, unix.AF_INET)
	if err != nil {
		return nil, fmt.Errorf("list host addresses: %w", err)
	}
	var out []string
	for _, a := range addrs {
		ip := a.IP.To4()
		if ip == nil || ip.IsLoopback() {
			continue
		}
		out = append(out, ip.String())
	}
	return out, nil
}

// ownMeshAddr is this node's own wg0 address, where the controller also
// listens once the mesh is joined; "" when there is none.
func ownMeshAddr() string {
	link, err := netlink.LinkByName("wg0")
	if err != nil {
		return ""
	}
	addrs, err := netlink.AddrList(link, unix.AF_INET)
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		if ip := a.IP.To4(); ip != nil && !ip.Equal(net.IPv4zero) {
			return ip.String()
		}
	}
	return ""
}

func (c *SpaceNet) applyHostGuard() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || !c.hostGuard {
		return nil
	}
	rep, err := c.send(command{Op: "hostguard", IF: slirpTap, HostAddrs: c.guardHostAddrs, ControllerAddrs: c.guardControllerAddrs, PublicAddrs: c.guardPublicAddrs, Ports: currentControllerPorts()})
	if err != nil {
		return err
	}
	if !rep.OK {
		return fmt.Errorf("%s", rep.Error)
	}
	return nil
}

// controllerPorts is the TCP port set the network controller serves, and the
// spaces whose guard follows it. The controller opens a listener per declared
// service port as services come and go, so every live space is re-programmed
// when the set changes.
var controllerPorts = struct {
	sync.Mutex
	ports []int
	nets  map[*SpaceNet]struct{}
}{nets: map[*SpaceNet]struct{}{}}

// SetControllerPorts records the TCP ports the network controller now serves
// and re-programs the host guard of every live space with them.
func SetControllerPorts(ports []int) {
	controllerPorts.Lock()
	controllerPorts.ports = append([]int(nil), ports...)
	nets := make([]*SpaceNet, 0, len(controllerPorts.nets))
	for n := range controllerPorts.nets {
		nets = append(nets, n)
	}
	controllerPorts.Unlock()
	for _, n := range nets {
		if err := n.applyHostGuard(); err != nil {
			fmt.Printf("[sklp] space %q host guard: %v\n", n.name, err)
		}
	}
}

func currentControllerPorts() []int {
	controllerPorts.Lock()
	defer controllerPorts.Unlock()
	return append([]int(nil), controllerPorts.ports...)
}

func trackHostGuard(c *SpaceNet, on bool) {
	controllerPorts.Lock()
	defer controllerPorts.Unlock()
	if on {
		controllerPorts.nets[c] = struct{}{}
	} else {
		delete(controllerPorts.nets, c)
	}
}
