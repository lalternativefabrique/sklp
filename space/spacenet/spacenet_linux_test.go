//go:build linux

package spacenet

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/internal/integtest"
)

// TestMain lets the re-exec'd test binary become the space worker or a
// netns holder when the markers are set; otherwise it runs the tests as
// usual. This is the same hook a real host main() installs via MaybeRunWorker.
func TestMain(m *testing.M) {
	MaybeRunWorker()      // no-op unless a worker/holder marker is set
	MaybeRunSubidRemove() // no-op unless the subuid-rm marker is set
	maybeRunSeedSubid()   // no-op unless the subuid-seed marker is set
	maybeRunSupervisorSim()
	os.Exit(m.Run())
}

// supervisorSimEnv marks a re-exec of the test binary that plays the role of a
// `sklp dev` supervisor: it builds a SpaceNet (forking the worker as its child),
// prints the worker pid on stdout, then parks. The parent test SIGKILLs it to
// assert the worker dies with it (Pdeathsig), reproducing the kill -9 leak.
const supervisorSimEnv = "SKLP_TEST_SUPERVISOR_SIM"

func maybeRunSupervisorSim() {
	if os.Getenv(supervisorSimEnv) == "" {
		return
	}
	cn, err := NewSpaceNet("killsim", "10.91.0.0/24")
	if err != nil {
		fmt.Fprintf(os.Stderr, "supervisor-sim: NewSpaceNet: %v\n", err)
		os.Exit(1)
	}
	// Neutralise the worker's secondary defence so the test isolates Pdeathsig.
	// The worker also exits when its stdin pipe reaches EOF (host gone), but EOF
	// only fires once EVERY writer fd is closed. In real `sklp dev` the supervisor
	// forks other children (native run:, slirp) that can inherit the worker's
	// stdin write end, so EOF never arrives on supervisor death and the worker
	// leaks — exactly the observed bug. We reproduce that by leaking the stdin
	// write end into a detached `sleep` that outlives us, leaving Pdeathsig as the
	// only thing that can reap the worker.
	if f, ok := cn.stdin.(*os.File); ok {
		// Short-lived: self-terminates so the test never leaks it. The window
		// (10s) comfortably outlasts the parent's reap deadline (5s).
		leak := exec.Command("sleep", "10")
		leak.Stdin = f // sleep inherits (dups) the worker's stdin write end
		leak.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		_ = leak.Start() // detached: survives our SIGKILL, holding the pipe open
	}
	// Hand the worker pid to the parent, then block forever. We deliberately do
	// NOT call Close: the parent will SIGKILL us, which is the whole point.
	fmt.Printf("%d\n", cn.worker.Process.Pid)
	_ = os.Stdout.Sync()
	select {}
}

// skipUnlessRootlessNet skips when this host cannot create a rootless
// user+net namespace (CI sandboxes, restricted kernels). The space worker
// depends on it, so without it there is nothing meaningful to assert.
func skipUnlessRootlessNet(t *testing.T) {
	t.Helper()
	out, err := exec.Command("unshare", "--user", "--net", "--map-root-user", "true").CombinedOutput()
	if err != nil {
		reason := strings.TrimSpace(string(out))
		integtest.Demand(t, "rootless user+net namespaces unavailable: "+reason)
		t.Skipf("rootless user+net namespaces unavailable here: %v: %s", err, reason)
	}
}

// TestSpaceNet_PingAcrossBridge is the go/no-go for space network step 1:
// two netns attached to the space bridge must reach each other by IP,
// rootless. It needs no runc — SpawnPeer models a container that joins the
// space userns with a private netns (the ownership shape Attach requires).
func TestSpaceNet_PingAcrossBridge(t *testing.T) {
	skipUnlessRootlessNet(t)

	cn, err := NewSpaceNet("test", "10.88.0.0/24")
	if err != nil {
		t.Fatalf("NewSpaceNet: %v", err)
	}
	defer cn.Close()

	ep := cn.Endpoints()
	if ep.Gateway != "10.88.0.1" {
		t.Fatalf("gateway = %s, want 10.88.0.1", ep.Gateway)
	}

	pidA, err := cn.SpawnPeer()
	if err != nil {
		t.Fatalf("SpawnPeer A: %v", err)
	}
	pidB, err := cn.SpawnPeer()
	if err != nil {
		t.Fatalf("SpawnPeer B: %v", err)
	}

	ipA, err := cn.Attach("svc-a", pidA, "eth0")
	if err != nil {
		t.Fatalf("Attach A (pid %d): %v", pidA, err)
	}
	ipB, err := cn.Attach("svc-b", pidB, "eth0")
	if err != nil {
		t.Fatalf("Attach B (pid %d): %v", pidB, err)
	}
	if ipA != "10.88.0.2" || ipB != "10.88.0.3" {
		t.Fatalf("IPAM gave A=%s B=%s, want .2/.3", ipA, ipB)
	}

	// The actual go/no-go: A reaches B by IP across the bridge.
	if err := cn.Ping(pidA, ipB); err != nil {
		t.Fatalf("A(%s)->B(%s) ping FAILED: %v", ipA, ipB, err)
	}
	if err := cn.Ping(pidB, ipA); err != nil {
		t.Fatalf("B(%s)->A(%s) ping FAILED: %v", ipB, ipA, err)
	}
}

// TestSpaceNet_DetachFreesSlot confirms Detach frees the address so the
// pool's net occupancy does not grow across an attach/detach/attach cycle:
// a detached IP must become allocatable again (the allocator reissues it on
// the next wrap — see TestIPAM_ReleaseRecycles for the ordering detail).
func TestSpaceNet_DetachFreesSlot(t *testing.T) {
	skipUnlessRootlessNet(t)

	cn, err := NewSpaceNet("recycle", "10.88.0.0/24")
	if err != nil {
		t.Fatalf("NewSpaceNet: %v", err)
	}
	defer cn.Close()

	pid, err := cn.SpawnPeer()
	if err != nil {
		t.Fatalf("SpawnPeer: %v", err)
	}
	ip1, err := cn.Attach("recycler", pid, "eth0")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if err := cn.Detach(pid); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	// After detach the address is back in the pool; the allocator must be
	// willing to hand it out again on wrap.
	if cn.ipam.inUse[ipToU32(parseTestIP(t, ip1))] {
		t.Fatalf("detach left %s marked in-use", ip1)
	}

	pid2, err := cn.SpawnPeer()
	if err != nil {
		t.Fatalf("SpawnPeer 2: %v", err)
	}
	if _, err := cn.Attach("recycler", pid2, "eth0"); err != nil {
		t.Fatalf("Attach 2: %v", err)
	}
}

// TestSpaceNet_DNSByName is the go/no-go for space network step 2: an
// attached peer resolves a sibling service by NAME to its bridge IP, by
// querying the in-space resolver on the gateway. No runc, no dig — SpawnPeer
// models the container and Resolve runs a Go DNS client in its netns.
func TestSpaceNet_DNSByName(t *testing.T) {
	skipUnlessRootlessNet(t)

	cn, err := NewSpaceNet("dns", "10.88.0.0/24")
	if err != nil {
		t.Fatalf("NewSpaceNet: %v", err)
	}
	defer cn.Close()

	// "client" peer issues the queries; "db" is the service it looks up.
	clientPID, err := cn.SpawnPeer()
	if err != nil {
		t.Fatalf("SpawnPeer client: %v", err)
	}
	dbPID, err := cn.SpawnPeer()
	if err != nil {
		t.Fatalf("SpawnPeer db: %v", err)
	}
	if _, err := cn.Attach("client", clientPID, "eth0"); err != nil {
		t.Fatalf("Attach client: %v", err)
	}
	dbIP, err := cn.Attach("db", dbPID, "eth0")
	if err != nil {
		t.Fatalf("Attach db: %v", err)
	}

	// The go/no-go: the client resolves "db" to db's bridge IP.
	ips, err := cn.Resolve(clientPID, "db")
	if err != nil {
		t.Fatalf("Resolve db: %v", err)
	}
	if len(ips) != 1 || ips[0] != dbIP {
		t.Fatalf("Resolve db = %v, want [%s]", ips, dbIP)
	}

	// A second replica of "db": both IPs must be returned (round-robin order).
	db2PID, err := cn.SpawnPeer()
	if err != nil {
		t.Fatalf("SpawnPeer db2: %v", err)
	}
	db2IP, err := cn.Attach("db", db2PID, "eth0")
	if err != nil {
		t.Fatalf("Attach db2: %v", err)
	}
	ips, err = cn.Resolve(clientPID, "db")
	if err != nil {
		t.Fatalf("Resolve db (2 replicas): %v", err)
	}
	if len(ips) != 2 || !contains(ips, dbIP) || !contains(ips, db2IP) {
		t.Fatalf("Resolve db = %v, want both %s and %s", ips, dbIP, db2IP)
	}

	// An unknown name forwards upstream; with no egress/upstream in this step
	// that fails (SERVFAIL) rather than resolving — assert it does NOT resolve
	// to a service IP. The forward PATH is exercised; success needs egress.
	if got, err := cn.Resolve(clientPID, "registry.npmjs.org"); err == nil && len(got) > 0 {
		if contains(got, dbIP) || contains(got, db2IP) {
			t.Fatalf("unknown name resolved to a service IP: %v", got)
		}
	}

	// Release one replica: "db" must now resolve to a single IP (the survivor).
	if err := cn.Detach(db2PID); err != nil {
		t.Fatalf("Detach db2: %v", err)
	}
	ips, err = cn.Resolve(clientPID, "db")
	if err != nil {
		t.Fatalf("Resolve db (after detach): %v", err)
	}
	if len(ips) != 1 || ips[0] != dbIP {
		t.Fatalf("after detach Resolve db = %v, want [%s]", ips, dbIP)
	}

	// Release the last replica: "db" must now NXDOMAIN/SERVFAIL, not resolve.
	if err := cn.Detach(dbPID); err != nil {
		t.Fatalf("Detach db: %v", err)
	}
	if got, err := cn.Resolve(clientPID, "db"); err == nil && contains(got, dbIP) {
		t.Fatalf("released service still resolves: %v", got)
	}
}

// TestSpaceNet_WorkerSubidRange is the go/no-go for the EINVAL fix: the
// worker's userns must map the FULL subuid range, not just our own uid. With
// the range in place, /proc/<worker>/uid_map has THREE columns per line and two
// lines — {0 -> uid, size 1} and {1 -> base, size count} — so a container
// joining the userns can chown to a non-root uid (e.g. nginx -> 101) instead of
// failing EINVAL. The base is whatever /etc/subuid grants this user (100000 for
// the first user, 165536 for the second, ...), so it is read from the same
// source the worker maps from rather than hardcoded. Skips when
// newuidmap/newgidmap or /etc/subuid are unavailable.
func TestSpaceNet_WorkerSubidRange(t *testing.T) {
	skipUnlessRootlessNet(t)
	if _, ok := lookupSubidTools(); !ok {
		t.Skip("newuidmap/newgidmap unavailable: range mapping not exercised")
	}

	cn, err := NewSpaceNet("subid", "10.88.0.0/24")
	if err != nil {
		// A host with the helpers but no /etc/subuid entry cannot map a range;
		// that surfaces as a start failure here, not a bug in the wiring.
		t.Skipf("NewSpaceNet (subuid not configured?): %v", err)
	}
	defer cn.Close()

	pid := cn.worker.Process.Pid
	uidMap, err := os.ReadFile(fmt.Sprintf("/proc/%d/uid_map", pid))
	if err != nil {
		t.Fatalf("read worker uid_map: %v", err)
	}
	uidBase, gidBase, count, err := resolveSubidRange(os.Getuid())
	if err != nil {
		t.Skipf("resolve subuid range: %v", err)
	}

	assertRangeMap(t, "uid_map", string(uidMap), os.Getuid(), uidBase, count)

	gidMap, err := os.ReadFile(fmt.Sprintf("/proc/%d/gid_map", pid))
	if err != nil {
		t.Fatalf("read worker gid_map: %v", err)
	}
	assertRangeMap(t, "gid_map", string(gidMap), os.Getgid(), gidBase, count)
}

// assertRangeMap checks an id_map has exactly the two expected lines: container
// 0 -> hostID size 1, and container 1 -> base size count. Each line is
// "<containerID> <hostID> <size>".
func assertRangeMap(t *testing.T, which, content string, hostID, base, count int) {
	t.Helper()
	var rows [][3]int
	for _, line := range strings.Split(strings.TrimSpace(content), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 {
			t.Fatalf("%s line %q: want 3 fields", which, line)
		}
		var r [3]int
		if _, err := fmt.Sscanf(line, "%d %d %d", &r[0], &r[1], &r[2]); err != nil {
			t.Fatalf("%s parse %q: %v", which, line, err)
		}
		rows = append(rows, r)
	}
	want := [][3]int{{0, hostID, 1}, {1, base, count}}
	if len(rows) != len(want) {
		t.Fatalf("%s = %v, want %v", which, rows, want)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Fatalf("%s row %d = %v, want %v", which, i, rows[i], want[i])
		}
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func parseTestIP(t *testing.T, s string) net.IP {
	t.Helper()
	ip := net.ParseIP(s)
	if ip == nil {
		t.Fatalf("parse ip %q", s)
	}
	return ip
}

// TestSpaceNet_CloseNoLeak verifies teardown leaves no space bridge on the
// host: the worker's netns is private, so the bridge never appears in the
// host's `ip link` and disappears entirely once the worker exits.
func TestSpaceNet_CloseNoLeak(t *testing.T) {
	skipUnlessRootlessNet(t)

	cn, err := NewSpaceNet("leak", "10.88.0.0/24")
	if err != nil {
		t.Fatalf("NewSpaceNet: %v", err)
	}
	bridge := cn.bridge
	if err := cn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Double close is a no-op.
	if err := cn.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	out, _ := exec.Command("ip", "link", "show").CombinedOutput()
	if strings.Contains(string(out), bridge) {
		t.Fatalf("space bridge %q leaked into host netns:\n%s", bridge, out)
	}
}

// TestSpaceNet_WorkerDiesWithSupervisorOnSIGKILL reproduces the kill -9 leak:
// a SIGKILL'd supervisor runs no teardown, so only the worker's Pdeathsig can
// reap it. We re-exec ourselves as a supervisor that builds a SpaceNet (forking
// the worker), SIGKILL the supervisor, and assert the worker pid is gone. Before
// the Pdeathsig fix the worker survived, reparented to PID 1, holding the bridge.
func TestSpaceNet_WorkerDiesWithSupervisorOnSIGKILL(t *testing.T) {
	skipUnlessRootlessNet(t)

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	sup := exec.Command(self)
	sup.Env = append(os.Environ(), supervisorSimEnv+"=1")
	sup.Stderr = os.Stderr
	stdout, err := sup.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if err := sup.Start(); err != nil {
		t.Fatalf("start supervisor-sim: %v", err)
	}
	defer func() {
		_ = sup.Process.Kill()
		_, _ = sup.Process.Wait()
	}()

	// Read the worker pid the supervisor-sim prints once its SpaceNet is up.
	var workerPID int
	if _, err := fmt.Fscanf(stdout, "%d\n", &workerPID); err != nil {
		t.Fatalf("read worker pid from supervisor-sim: %v", err)
	}
	if workerPID <= 1 {
		t.Fatalf("implausible worker pid %d", workerPID)
	}
	if !pidAlive(workerPID) {
		t.Fatalf("worker pid %d not alive before SIGKILL", workerPID)
	}

	// SIGKILL the supervisor: it cannot run Close, so the worker survives iff
	// Pdeathsig is not wired. Kernel delivers SIGKILL to the worker on our death.
	if err := sup.Process.Kill(); err != nil {
		t.Fatalf("SIGKILL supervisor-sim: %v", err)
	}

	// Poll for the worker to vanish. Pdeathsig is delivered on parent death; the
	// worker then exits and is reaped by PID 1. Allow a generous window.
	deadline := 5 * time.Second
	step := 20 * time.Millisecond
	for waited := time.Duration(0); waited < deadline; waited += step {
		if !pidAlive(workerPID) {
			return // worker reaped with the supervisor — fix holds
		}
		time.Sleep(step)
	}
	t.Fatalf("worker pid %d still alive %s after supervisor SIGKILL — Pdeathsig leak", workerPID, deadline)
}
