//go:build linux

package spacenet

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/space/ociproc"
)

func requireChildUserns(t *testing.T) *SpaceNet {
	t.Helper()
	if _, ok := lookupSubidTools(); !ok {
		t.Skip("newuidmap/newgidmap unavailable: child userns needs a delegated range")
	}
	skipUnlessRootlessNetns(t)
	openExecutableToOthers(t)
	cn, err := NewSpaceNet("qa"+t.Name()[len(t.Name())-6:], "10.94.0.0/24")
	if err != nil {
		t.Skipf("NewSpaceNet: %v", err)
	}
	return cn
}

// overlayDirs makes a lower/upper/work/merged set reachable by an unmapped id
// and removes it, whoever ended up owning its files, when the test ends.
func overlayDirs(t *testing.T, name string) (lower, upper, work, merged string) {
	t.Helper()
	base := t.TempDir()
	t.Cleanup(func() { _ = RemoveAllAsRoot(base) })
	for d := base; strings.HasPrefix(d, os.TempDir()) && d != os.TempDir(); d = filepath.Dir(d) {
		if err := os.Chmod(d, 0o711); err != nil {
			t.Fatal(err)
		}
	}
	base = filepath.Join(base, name)
	lower, upper, work, merged = filepath.Join(base, "lower"), filepath.Join(base, "upper"), filepath.Join(base, "work"), filepath.Join(base, "merged")
	for _, d := range []string{lower, upper, work, merged} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return
}

func processGone(pid int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); err != nil {
			return true
		}
		if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil && strings.Contains(string(b), ") Z ") {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// The kernel clears PR_SET_PDEATHSIG when a process gains capabilities, which
// a child-userns holder does at unshare and at its mapped re-exec. Without the
// re-arm the holder outlives the worker as an orphan the operator can neither
// see (its environ is unreadable) nor kill (it runs as a subuid).
func TestChildUserns_HolderDiesWithTheWorker(t *testing.T) {
	cn := requireChildUserns(t)
	lower, upper, work, merged := overlayDirs(t, "svc")
	pid, _, err := cn.SpawnOverlayChild(lower, upper, work, merged, 0)
	if err != nil {
		t.Fatalf("SpawnOverlayChild: %v", err)
	}
	waitOverlayMounted(t, pid, merged)

	cn.Close()
	if !processGone(pid, 3*time.Second) {
		t.Fatalf("child holder %d survived its worker", pid)
	}
}

// Identity must describe the map the worker actually carries, not a second
// reading of /etc/subuid that could disagree with it.
func TestSpaceNet_IdentityIsTheWorkerMap(t *testing.T) {
	cn := requireChildUserns(t)
	defer cn.Close()
	id := cn.Identity()
	uidMap, err := os.ReadFile(fmt.Sprintf("/proc/%d/uid_map", cn.worker.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	gidMap, err := os.ReadFile(fmt.Sprintf("/proc/%d/gid_map", cn.worker.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	wantUID := fmt.Sprintf("0 %d 1 1 %d %d", id.HostUID, id.UIDBase, id.UIDCount)
	wantGID := fmt.Sprintf("0 %d 1 1 %d %d", id.HostGID, id.GIDBase, id.GIDCount)
	if got := squash(string(uidMap)); got != wantUID {
		t.Fatalf("worker uid_map %q, Identity says %q", got, wantUID)
	}
	if got := squash(string(gidMap)); got != wantGID {
		t.Fatalf("worker gid_map %q, Identity says %q", got, wantGID)
	}
	if id.HostUID != os.Getuid() {
		t.Fatalf("space root is %d, want the operator %d", id.HostUID, os.Getuid())
	}
}

// The shared-userns holder is the path every deployment runs today: runc must
// still be entered with the operator's credentials kept, exactly as before.
func TestSpaceNet_SharedHolderKeepsCredentials(t *testing.T) {
	cn := requireChildUserns(t)
	defer cn.Close()
	lower, upper, work, merged := overlayDirs(t, "shared")
	pid, err := cn.SpawnOverlay(lower, upper, work, merged)
	if err != nil {
		t.Fatalf("SpawnOverlay: %v", err)
	}
	waitOverlayMounted(t, pid, merged)
	args := strings.Join(ociproc.EnterArgs(pid, true), " ")
	if !strings.HasSuffix(args, "--mount --preserve-credentials") || strings.Contains(args, "-S") {
		t.Fatalf("shared holder EnterArgs = %q", args)
	}
	out, err := exec.Command("nsenter", append(ociproc.EnterArgs(pid, true), "--", "id", "-u")...).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "0" {
		t.Fatalf("entering the shared holder: %v: %s", err, out)
	}
	// The upper stays the operator's, as it is today.
	st, err := os.Stat(upper)
	if err != nil {
		t.Fatal(err)
	}
	if got := int(st.Sys().(*syscall.Stat_t).Uid); got != os.Getuid() {
		t.Fatalf("shared upper owned by %d, want the operator %d", got, os.Getuid())
	}
}

// Two services of one space get one identity: the same child root, so a
// space stays one security domain and its volumes one owner.
func TestChildUserns_TwoServicesShareOneRoot(t *testing.T) {
	cn := requireChildUserns(t)
	defer cn.Close()
	l1, u1, w1, m1 := overlayDirs(t, "a")
	l2, u2, w2, m2 := overlayDirs(t, "b")
	p1, id1, err := cn.SpawnOverlayChild(l1, u1, w1, m1, 0)
	if err != nil {
		t.Fatal(err)
	}
	p2, id2, err := cn.SpawnOverlayChild(l2, u2, w2, m2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Fatalf("identities differ: %+v vs %+v", id1, id2)
	}
	waitOverlayMounted(t, p1, m1)
	waitOverlayMounted(t, p2, m2)
	u1ns, _ := os.Readlink(fmt.Sprintf("/proc/%d/ns/user", p1))
	u2ns, _ := os.Readlink(fmt.Sprintf("/proc/%d/ns/user", p2))
	if u1ns == u2ns || u1ns == "" {
		t.Fatalf("holders must each own a user namespace, got %q and %q", u1ns, u2ns)
	}
}

// Without newuidmap the space falls back to the single-id map: Identity says
// so and a child userns is refused instead of mapping garbage.
func TestSpaceNet_NoSubidToolsFallsBackToSingleIdentity(t *testing.T) {
	skipUnlessRootlessNetns(t)
	t.Setenv("PATH", t.TempDir())
	cn, err := NewSpaceNet("qanotools", "10.95.0.0/24")
	if err != nil {
		t.Skipf("NewSpaceNet without tools: %v", err)
	}
	defer cn.Close()
	if got, want := cn.Identity(), ociproc.SingleIdentity(os.Getuid(), os.Getgid()); got != want {
		t.Fatalf("fallback identity = %+v, want %+v", got, want)
	}
	if _, _, err := cn.SpawnOverlayChild("/nonexistent", "/nonexistent", "/nonexistent", "/nonexistent", 0); err == nil || !strings.Contains(err.Error(), "delegated") {
		t.Fatalf("child userns without a range must be refused, got %v", err)
	}
}

// Two spaces of one node take different slots, and with them different roots
// that map each other's root to nobody.
func TestChildUserns_SlotsGiveDistinctRoots(t *testing.T) {
	cn := requireChildUserns(t)
	defer cn.Close()
	l1, u1, w1, m1 := overlayDirs(t, "s0")
	l2, u2, w2, m2 := overlayDirs(t, "s1")
	p1, id1, err := cn.SpawnOverlayChild(l1, u1, w1, m1, 0)
	if err != nil {
		t.Fatal(err)
	}
	p2, id2, err := cn.SpawnOverlayChild(l2, u2, w2, m2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if id1.HostUID == id2.HostUID || id1.HostUID != id2.HostUID+1 {
		t.Fatalf("slots 0 and 1 gave roots %d and %d", id1.HostUID, id2.HostUID)
	}
	waitOverlayMounted(t, p1, m1)
	waitOverlayMounted(t, p2, m2)
	for _, c := range []struct {
		pid  int
		root int
	}{{p1, id1.HostUID}, {p2, id2.HostUID}} {
		m, _ := os.ReadFile(fmt.Sprintf("/proc/%d/uid_map", c.pid))
		if !strings.Contains(squash(string(m)), fmt.Sprintf("0 %d 1", c.root)) {
			t.Fatalf("holder %d uid_map %q lacks root %d", c.pid, squash(string(m)), c.root)
		}
	}
	if _, _, err := cn.SpawnOverlayChild(l1, u1, w1, m1, childRootSlotsTooMany); err == nil {
		t.Fatal("a slot beyond the range must be refused")
	}
	if id1.UIDCount != cn.Identity().UIDCount-ChildRootSlots {
		t.Fatalf("child maps %d ids, want the range minus the %d root slots", id1.UIDCount, ChildRootSlots)
	}
	// Neither root can read what the other wrote: each is nobody to the other.
	enter := func(pid int, args ...string) (string, error) {
		out, err := exec.Command("nsenter", append(append(ociproc.EnterArgs(pid, true), "--"), args...)...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if _, err := enter(p2, "sh", "-c", "echo s > "+m2+"/secret && chmod 600 "+m2+"/secret"); err != nil {
		t.Fatalf("write in space 2: %v", err)
	}
	if _, err := enter(p1, "sh", "-c", "echo s > "+m1+"/secret && chmod 600 "+m1+"/secret"); err != nil {
		t.Fatalf("write in space 1: %v", err)
	}
	secret2 := filepath.Join(u2, "secret")
	secret1 := filepath.Join(u1, "secret")
	if out, _ := enter(p1, "sh", "-c", "test -r "+secret2+" && echo READABLE || echo denied"); out != "denied" {
		t.Fatalf("space 1 root reads space 2's file: %s", out)
	}
	if out, _ := enter(p2, "sh", "-c", "test -r "+secret1+" && echo READABLE || echo denied"); out != "denied" {
		t.Fatalf("space 2 root reads space 1's file: %s", out)
	}
}

const childRootSlotsTooMany = 1 << 20

// The attach socket of a child-userns space lives outside the 0700 runtime
// dir, where its root can reach it. Its directory must then admit exactly the
// operator and that root: group of the child root, mode 0710, socket 0600.
func TestChildUserns_AttachSocketDirAdmitsOnlyOperatorAndRoot(t *testing.T) {
	if _, ok := lookupSubidTools(); !ok {
		t.Skip("newuidmap/newgidmap unavailable")
	}
	skipUnlessRootlessNetns(t)
	openExecutableToOthers(t)
	dir := filepath.Join(t.TempDir(), "space")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cn, err := NewSpaceNetAt("qasock", "10.97.0.0/24", filepath.Join(dir, "attach.sock"))
	if err != nil {
		t.Skipf("NewSpaceNetAt: %v", err)
	}
	defer cn.Close()
	lower, upper, work, merged := overlayDirs(t, "svc")
	_, id, err := cn.SpawnOverlayChild(lower, upper, work, merged, 0)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o710 || int(st.Sys().(*syscall.Stat_t).Gid) != id.HostGID {
		t.Fatalf("socket dir %s: mode %#o gid %d, want 0710 and the child root gid %d", dir, st.Mode().Perm(), st.Sys().(*syscall.Stat_t).Gid, id.HostGID)
	}
	sock, err := os.Stat(filepath.Join(dir, "attach.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if sock.Mode().Perm() != 0o600 || int(sock.Sys().(*syscall.Stat_t).Uid) != id.HostUID {
		t.Fatalf("socket: mode %#o uid %d, want 0600 owned by %d", sock.Mode().Perm(), sock.Sys().(*syscall.Stat_t).Uid, id.HostUID)
	}
}
