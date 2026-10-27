//go:build linux

package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lalternativefabrique/sklp/cockpit"
)

// readVMState collects what the guest can tell about the machine it runs on.
// Every step is best-effort: on a host where a file is absent or unreadable
// the field stays zero and the panel simply shows less, because a cockpit
// that fails to refresh is worse than one missing a number.
func readVMState() cockpit.VMState {
	var v cockpit.VMState

	v.Backend = os.Getenv("SKLP_VM_BACKEND")
	v.Kernel = firstLine("/proc/sys/kernel/osrelease")
	v.CPUs = countCPUs()
	v.LoadAvg = readLoadAvg()
	v.MemTotal, v.MemAvail = readMeminfo()
	v.Uptime = readUptime()
	v.Addrs = readAddrs()
	v.Processes = readProcesses()
	return v
}

func firstLine(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func countCPUs() int {
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "processor") {
			n++
		}
	}
	return n
}

func readLoadAvg() [3]float64 {
	var out [3]float64
	fields := strings.Fields(firstLine("/proc/loadavg"))
	for i := 0; i < 3 && i < len(fields); i++ {
		out[i], _ = strconv.ParseFloat(fields[i], 64)
	}
	return out
}

// readMeminfo returns total and available memory in bytes. MemAvailable is
// the kernel's own estimate of what a new workload could claim — a better
// answer than total-minus-free, which counts reclaimable cache as used.
func readMeminfo() (total, avail uint64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, value, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		switch key {
		case "MemTotal":
			total = parseKB(value)
		case "MemAvailable":
			avail = parseKB(value)
		}
		if total > 0 && avail > 0 {
			break
		}
	}
	return total, avail
}

func parseKB(field string) uint64 {
	fields := strings.Fields(field)
	if len(fields) == 0 {
		return 0
	}
	kb, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return 0
	}
	return kb * 1024
}

func readUptime() time.Duration {
	fields := strings.Fields(firstLine("/proc/uptime"))
	if len(fields) == 0 {
		return 0
	}
	secs, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// readAddrs lists the guest's IPv4 addresses, interface by interface. The
// netlink lookup behind net.InterfaceByName is what the guest agent already
// uses successfully; parsing /proc/net/fib_trie instead was tried and read
// nothing here, so this stays on the path that is known to answer.
func readAddrs() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.To4() == nil {
				continue // IPv6: the link-local address is noise here
			}
			out = append(out, iface.Name+" "+ipnet.String())
		}
	}
	return out
}

// readProcesses lists the workload running in the VM, heaviest first. Kernel
// threads are skipped: they are the same on every box and would bury the
// handful of processes the user actually put there.
func readProcesses() []cockpit.VMProcess {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []cockpit.VMProcess
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		name, rss, ok := procStat(pid)
		if !ok || rss == 0 {
			continue
		}
		out = append(out, cockpit.VMProcess{PID: pid, Name: name, RSS: rss})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RSS > out[j].RSS })
	if len(out) > vmProcessLimit {
		out = out[:vmProcessLimit]
	}
	return out
}

// vmProcessLimit caps the list so the panel stays a summary. The processes
// are sorted by resident memory first, so what is dropped is the tail.
const vmProcessLimit = 12

// procStat reads a process's name and resident size. A zero RSS means a
// kernel thread, which the caller drops.
func procStat(pid int) (name string, rss uint64, ok bool) {
	dir := filepath.Join("/proc", strconv.Itoa(pid))

	// comm rather than the stat line: the name there is parenthesised and can
	// itself contain spaces and brackets, which makes field-splitting wrong.
	comm, err := os.ReadFile(filepath.Join(dir, "comm"))
	if err != nil {
		return "", 0, false
	}
	name = strings.TrimSpace(string(comm))

	statm, err := os.ReadFile(filepath.Join(dir, "statm"))
	if err != nil {
		return "", 0, false
	}
	fields := strings.Fields(string(statm))
	if len(fields) < 2 {
		return "", 0, false
	}
	pages, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return name, pages * uint64(os.Getpagesize()), true
}

// streamVMLogs tails the guest agent's log, following it the way the CI log
// seam does: the agent keeps appending as the VM runs.
func streamVMLogs(ctx context.Context, out func(string)) error {
	const path = "/run/sklp-vzagent.log"
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("no agent log at %s (this host is not a vz guest?): %w", path, err)
	}
	defer f.Close()

	out("── tailing " + path + " ──")
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadString('\n')
		if line != "" {
			out(strings.TrimRight(line, "\n"))
		}
		if err == nil {
			continue
		}
		if err != io.EOF {
			return err
		}
		// EOF only means "nothing more yet": the agent appends for as long as
		// the VM lives, so wait rather than close the panel.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}
