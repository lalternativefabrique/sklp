//go:build integration_runtime && linux

// Real space-network integration test for the deploy launcher's wiring. It
// is NOT in the default suite: it needs rootless user+net namespaces (and,
// for the egress half, /dev/net/tun + nft). Run it explicitly with:
//
//	go test -tags 'integration_runtime embedded_runtime' \
//	    -run TestSpaceNet_TwoServicesResolveByName -buildvcs=false ./runtime/
//
// It models the go/no-go scenario — two space services where one reaches the
// other BY NAME — without pulling real images or invoking runc: spacenet's
// SpawnPeer forks a process that joins the space user namespace with a
// PRIVATE network namespace (exactly the namespace shape buildServiceSpec
// produces for a space service via join().PrivateNetns), Attach wires it onto
// the bridge, and Resolve runs a Go DNS client inside the peer's netns against
// the space resolver. A full runc variant is the manual command documented
// above plus a real `sklp deploy` of a 2-service space.
package space

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/space/spacenet"
)

// TestMain lets the re-exec'd test binary become the space worker / netns
// holder when spacenet re-execs it — the same hook a real host main() installs
// via maybeRunSpaceNetWorker(). No-op for the normal test process.
func TestMain(m *testing.M) {
	spacenet.MaybeRunWorker()
	spacenet.MaybeRunSubidRemove()
	os.Exit(m.Run())
}

func requireUserns(t *testing.T) {
	t.Helper()
	// Probe rootless user+net namespaces with `unshare -Urn true`: if it fails
	// the host forbids them (no CAP, sysctl off) and the test cannot run.
	out, err := exec.Command("unshare", "-Urn", "true").CombinedOutput()
	if err != nil {
		t.Skipf("rootless user+net namespaces unavailable here: %v: %s", err, strings.TrimSpace(string(out)))
	}
}

// TestSpaceNet_TwoServicesResolveByName builds the space network through
// the deploy launcher's own wrapper (newSpaceNet), then proves a "client"
// service resolves a "db" service BY NAME over the space bridge — the final
// network milestone's go/no-go, exercised on the real runtime glue.
func TestSpaceNet_TwoServicesResolveByName(t *testing.T) {
	requireUserns(t)

	cn, err := newSpaceNet(context.Background(), "itest", "", false, nil, "")
	if err != nil {
		t.Fatalf("newSpaceNet: %v", err)
	}
	defer cn.close()

	// The join the launcher hands runc: space userns + private netns. Asserts
	// the wrapper exposes the space user namespace path (the kernel-stable
	// handle a real container joins).
	if j := cn.join(); j == nil || j.UsernsPath == "" || !j.PrivateNetns {
		t.Fatalf("space join must be userns-by-path + private netns, got %+v", j)
	}
	if cn.resolvConf() == "" {
		t.Fatal("space must expose a resolv.conf path to bind-mount")
	}
	// A space netns ships no /etc/hosts; the launcher must bind-mount a
	// generated one with at least loopback, else nats-like images crash on boot.
	hostsPath := cn.hostsConf()
	if hostsPath == "" {
		t.Fatal("space must expose an /etc/hosts path to bind-mount")
	}
	if b, rerr := os.ReadFile(hostsPath); rerr != nil || !strings.Contains(string(b), "127.0.0.1") {
		t.Fatalf("generated /etc/hosts must contain loopback, got %q (err %v)", string(b), rerr)
	}

	// Model two services as spacenet peers (each joins the space userns with a
	// private netns, exactly like a runc space service).
	dbPID, err := cn.cnet.SpawnPeer()
	if err != nil {
		t.Fatalf("spawn db: %v", err)
	}
	clientPID, err := cn.cnet.SpawnPeer()
	if err != nil {
		t.Fatalf("spawn client: %v", err)
	}

	dbIP, err := cn.attach("db", dbPID)
	if err != nil {
		t.Fatalf("attach db: %v", err)
	}
	if _, err := cn.attach("client", clientPID); err != nil {
		t.Fatalf("attach client: %v", err)
	}

	ips, err := cn.cnet.Resolve(clientPID, "db")
	if err != nil {
		t.Fatalf("resolve db from client: %v", err)
	}
	if len(ips) != 1 || ips[0] != dbIP {
		t.Fatalf("resolve db = %v, want [%s]", ips, dbIP)
	}

	// L3 reachability over the bridge: client can reach db's IP.
	if err := cn.cnet.Ping(clientPID, dbIP); err != nil {
		t.Fatalf("ping db (%s) from client: %v", dbIP, err)
	}

	// Multi-replica: a second db instance registers the SAME name; resolve now
	// round-robins both IPs (the jalon-3 instance mechanism applied to spacenet).
	db2PID, err := cn.cnet.SpawnPeer()
	if err != nil {
		t.Fatalf("spawn db2: %v", err)
	}
	db2IP, err := cn.attach("db", db2PID)
	if err != nil {
		t.Fatalf("attach db2: %v", err)
	}
	ips, err = cn.cnet.Resolve(clientPID, "db")
	if err != nil {
		t.Fatalf("resolve db (2 replicas): %v", err)
	}
	if !(contains(ips, dbIP) && contains(ips, db2IP)) {
		t.Fatalf("resolve db = %v, want both %s and %s", ips, dbIP, db2IP)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
