package spacemanager

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
)

// tcpEstablished is the st column value of an ESTABLISHED socket in
// /proc/<pid>/net/tcp (include/net/tcp_states.h).
const tcpEstablished = "01"

// establishedConns counts the TCP connections established to ip:port as seen
// from pid's network namespace. The kernel keeps the table; nothing is asked
// of the process.
func establishedConns(pid int, ip string, port int) (int, error) {
	total := 0
	for _, table := range []string{"tcp", "tcp6"} {
		f, err := os.Open(fmt.Sprintf("/proc/%d/net/%s", pid, table))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return 0, err
		}
		n, err := countEstablished(f, ip, port)
		f.Close()
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

func countEstablished(r io.Reader, ip string, port int) (int, error) {
	want := net.ParseIP(ip)
	if want == nil {
		return 0, fmt.Errorf("invalid ip %q", ip)
	}
	sc := bufio.NewScanner(r)
	n := 0
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 || fields[0] == "sl" {
			continue
		}
		if fields[3] != tcpEstablished {
			continue
		}
		addr, p, ok := parseProcAddr(fields[1])
		if ok && p == port && addr.Equal(want) {
			n++
		}
	}
	return n, sc.Err()
}

// parseProcAddr decodes the "<hex ip>:<hex port>" form of /proc/net/tcp{,6}.
// The address is written in host byte order per 32-bit word, so each word is
// reversed to recover network order.
func parseProcAddr(s string) (net.IP, int, bool) {
	i := strings.LastIndexByte(s, ':')
	if i < 0 {
		return nil, 0, false
	}
	raw, err := hex.DecodeString(s[:i])
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return nil, 0, false
	}
	port, err := strconv.ParseInt(s[i+1:], 16, 32)
	if err != nil {
		return nil, 0, false
	}
	for w := 0; w < len(raw); w += 4 {
		raw[w], raw[w+1], raw[w+2], raw[w+3] = raw[w+3], raw[w+2], raw[w+1], raw[w]
	}
	return net.IP(raw), int(port), true
}
