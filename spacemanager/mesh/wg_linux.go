package mesh

import (
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

const (
	// InterfaceName is the host-wide WireGuard mesh interface. One per node,
	// shared by every space — the mesh is a host concept, not a per-space one.
	InterfaceName = "wg0"
	listenPort    = 51820
)

// EnsureInterface creates wg0 if it does not already exist and assigns it
// selfIP (e.g. "10.90.0.5/24", meshauthority's allocation for this node).
// Idempotent: safe to call on every boot. Any OTHER IPv4 address on wg0 is
// removed: the interface outlives the agent process, so after an identity
// reset (new registration, new allocation) the previous address would
// otherwise linger as a stale secondary — observed live as .3 + .4 both on
// wg0 after a mesh identity wipe.
func EnsureInterface(selfCIDR string) error {
	link, err := netlink.LinkByName(InterfaceName)
	if err != nil {
		if _, ok := err.(netlink.LinkNotFoundError); !ok {
			return fmt.Errorf("lookup %s: %w", InterfaceName, err)
		}
		wgLink := &netlink.Wireguard{LinkAttrs: netlink.LinkAttrs{Name: InterfaceName}}
		if err := netlink.LinkAdd(wgLink); err != nil {
			return fmt.Errorf("create %s: %w", InterfaceName, err)
		}
		link, err = netlink.LinkByName(InterfaceName)
		if err != nil {
			return fmt.Errorf("lookup %s after create: %w", InterfaceName, err)
		}
	}

	addr, err := netlink.ParseAddr(selfCIDR)
	if err != nil {
		return fmt.Errorf("parse mesh address %q: %w", selfCIDR, err)
	}
	existing, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("list %s addresses: %w", InterfaceName, err)
	}
	hasAddr := false
	for _, a := range existing {
		if a.Equal(*addr) {
			hasAddr = true
			continue
		}
		if err := netlink.AddrDel(link, &a); err != nil {
			return fmt.Errorf("remove stale %s from %s: %w", a.IPNet, InterfaceName, err)
		}
	}
	if !hasAddr {
		if err := netlink.AddrAdd(link, addr); err != nil {
			return fmt.Errorf("assign %s to %s: %w", selfCIDR, InterfaceName, err)
		}
	}

	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("bring up %s: %w", InterfaceName, err)
	}
	return nil
}

// SyncPeers replaces wg0's full peer set with peers — the same
// whole-document-replace pattern meshauthority itself uses. self is
// excluded even if present in peers (a node is never its own WireGuard
// peer). A peer with no endpoint is still configured (its allowed-ips
// route works once it dials in), just unable to be dialed first.
func SyncPeers(privateKey wgtypes.Key, selfID string, peers []Peer) error {
	client, err := wgctrl.New()
	if err != nil {
		return fmt.Errorf("open wgctrl: %w", err)
	}
	defer client.Close()

	port := listenPort
	cfg := wgtypes.Config{
		PrivateKey:   &privateKey,
		ListenPort:   &port,
		ReplacePeers: true,
	}

	for _, p := range peers {
		if p.ID == selfID {
			continue
		}
		pubKey, err := wgtypes.ParseKey(p.PubKey)
		if err != nil {
			return fmt.Errorf("parse pubkey for peer %s: %w", p.ID, err)
		}

		peerCfg := wgtypes.PeerConfig{
			PublicKey: pubKey,
			AllowedIPs: []net.IPNet{{
				IP:   net.ParseIP(p.PrivateIP),
				Mask: net.CIDRMask(32, 32),
			}},
			ReplaceAllowedIPs: true,
		}
		if p.Endpoint != "" {
			endpoint, err := net.ResolveUDPAddr("udp", p.Endpoint)
			if err != nil {
				return fmt.Errorf("resolve endpoint for peer %s: %w", p.ID, err)
			}
			peerCfg.Endpoint = endpoint
		}
		cfg.Peers = append(cfg.Peers, peerCfg)
	}

	if err := client.ConfigureDevice(InterfaceName, cfg); err != nil {
		return fmt.Errorf("configure %s: %w", InterfaceName, err)
	}
	return nil
}
