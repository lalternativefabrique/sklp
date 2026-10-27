package publicnic

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const LeaseDir = "/run/systemd/netif/leases"

// Lease is the part of a systemd-networkd DHCP lease the reconciler needs:
// the NIC takes no route from DHCP, so the gateway exists only here.
type Lease struct {
	Address net.IP
	Router  net.IP
}

func ParseLease(r io.Reader) (Lease, error) {
	var l Lease
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		key, val, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok {
			continue
		}
		switch key {
		case "ADDRESS":
			l.Address = parseIPv4(val)
		case "ROUTER":
			if fields := strings.Fields(val); len(fields) > 0 {
				l.Router = parseIPv4(fields[0])
			}
		}
	}
	if err := sc.Err(); err != nil {
		return Lease{}, err
	}
	if l.Router == nil {
		return Lease{}, fmt.Errorf("lease has no ROUTER")
	}
	if l.Address == nil {
		return Lease{}, fmt.Errorf("lease has no ADDRESS")
	}
	return l, nil
}

func ReadLease(dir string, linkIndex int) (Lease, error) {
	path := filepath.Join(dir, strconv.Itoa(linkIndex))
	f, err := os.Open(path)
	if err != nil {
		return Lease{}, err
	}
	defer f.Close()
	l, err := ParseLease(f)
	if err != nil {
		return Lease{}, fmt.Errorf("%s: %w", path, err)
	}
	return l, nil
}

func parseIPv4(s string) net.IP {
	if ip := net.ParseIP(strings.TrimSpace(s)); ip != nil {
		return ip.To4()
	}
	return nil
}
