//go:build linux

package publicnic

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

type netlinkHost struct{ leaseDir string }

func NewHost() (Host, error) { return netlinkHost{leaseDir: LeaseDir}, nil }

func (h netlinkHost) LinkIndex(name string) (int, error) {
	l, err := netlink.LinkByName(name)
	if err != nil {
		return 0, err
	}
	return l.Attrs().Index, nil
}

func (h netlinkHost) Lease(linkIndex int) (Lease, error) { return ReadLease(h.leaseDir, linkIndex) }

func (h netlinkHost) Observe(d Desired) (Observed, error) {
	var o Observed
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: d.Table}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return o, fmt.Errorf("list routes table %d: %w", d.Table, err)
	}
	for _, rt := range routes {
		if rt.Dst != nil && !isDefault(rt.Dst) {
			continue
		}
		o.DefaultRoute = &RouteState{Gateway: rt.Gw, LinkIndex: rt.LinkIndex, OnLink: rt.Flags&int(netlink.FLAG_ONLINK) != 0}
		break
	}
	rules, err := netlink.RuleListFiltered(netlink.FAMILY_V4, &netlink.Rule{Table: d.Table}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return o, fmt.Errorf("list rules table %d: %w", d.Table, err)
	}
	for _, ru := range rules {
		if ru.Invert {
			continue
		}
		if ru.Mark == 0 && ru.Src != nil && ru.Src.IP.Equal(d.Source) && isHost(ru.Src) {
			o.RuleFrom = true
		}
		if ru.Mark == d.Mark && ru.Src == nil && (ru.Mask == nil || *ru.Mask == 0xffffffff) {
			o.RuleMark = true
		}
	}
	listing, err := nftList()
	if err != nil {
		return o, err
	}
	o.NftMissing = NftMissing(listing, d)
	return o, nil
}

func (h netlinkHost) Apply(d Desired, r Repair) error {
	switch r.Action {
	case ActionRoute:
		return netlink.RouteReplace(&netlink.Route{
			Table:     d.Table,
			LinkIndex: d.LinkIndex,
			Gw:        d.Gateway,
			Dst:       &net.IPNet{IP: net.IPv4zero.To4(), Mask: net.CIDRMask(0, 32)},
			Flags:     int(netlink.FLAG_ONLINK),
		})
	case ActionRuleFrom:
		rule := netlink.NewRule()
		rule.Family = netlink.FAMILY_V4
		rule.Table = d.Table
		rule.Src = &net.IPNet{IP: d.Source, Mask: net.CIDRMask(32, 32)}
		return netlink.RuleAdd(rule)
	case ActionRuleMark:
		rule := netlink.NewRule()
		rule.Family = netlink.FAMILY_V4
		rule.Table = d.Table
		rule.Mark = d.Mark
		return netlink.RuleAdd(rule)
	case ActionNft:
		cmd := exec.Command("nft", "-f", "-")
		cmd.Stdin = strings.NewReader(NftScript(d))
		if out, err := nftOutput(cmd); err != nil {
			return fmt.Errorf("nft -f: %w (%s)", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	return fmt.Errorf("unknown repair %q", r.Action)
}

func nftList() (string, error) {
	out, err := nftOutput(exec.Command("nft", "list", "table", "ip", NftTable))
	if err == nil {
		return string(out), nil
	}
	if strings.Contains(string(out), "No such file or directory") {
		return "", nil
	}
	return "", fmt.Errorf("nft list table ip %s: %w (%s)", NftTable, err, strings.TrimSpace(string(out)))
}

func isDefault(n *net.IPNet) bool {
	ones, _ := n.Mask.Size()
	return ones == 0 && n.IP.IsUnspecified()
}

func isHost(n *net.IPNet) bool {
	ones, bits := n.Mask.Size()
	return ones == bits
}

// Events fires on any link or route change, coalesced over a short window so
// one networkd restart triggers one reconcile, not hundreds.
func Events(ctx context.Context) (<-chan struct{}, error) {
	routes := make(chan netlink.RouteUpdate, 64)
	links := make(chan netlink.LinkUpdate, 64)
	if err := netlink.RouteSubscribe(routes, ctx.Done()); err != nil {
		return nil, fmt.Errorf("subscribe routes: %w", err)
	}
	if err := netlink.LinkSubscribe(links, ctx.Done()); err != nil {
		return nil, fmt.Errorf("subscribe links: %w", err)
	}
	out := make(chan struct{}, 1)
	go func() {
		var fire <-chan time.Time
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-routes:
				if !ok {
					return
				}
				fire = coalesce(fire)
			case _, ok := <-links:
				if !ok {
					return
				}
				fire = coalesce(fire)
			case <-fire:
				fire = nil
				select {
				case out <- struct{}{}:
				default:
				}
			}
		}
	}()
	return out, nil
}

func coalesce(pending <-chan time.Time) <-chan time.Time {
	if pending != nil {
		return pending
	}
	return time.After(2 * time.Second)
}

// nftOutput is CombinedOutput tolerating ECHILD: the sklet supervisor reaps
// every child from its SIGCHLD drain loop, and when it wins the race against
// this wait the status is gone although nft ran and its output is complete.
func nftOutput(cmd *exec.Cmd) ([]byte, error) {
	out, err := cmd.CombinedOutput()
	if err != nil && errors.Is(err, unix.ECHILD) {
		return out, nil
	}
	return out, err
}
