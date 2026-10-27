//go:build linux

package spacenet

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/space/ociproc"
)

// TestChildUserns_ServiceRootIsASubuid is the go/no-go of the userns
// redesign (Skalpai issue 135576e3): a service in a user namespace that is a
// CHILD of the space worker's. It proves, on the real worker:
//   - the worker writes the child's maps and uid 0 there is a subuid;
//   - what that root writes lands on the host owned by that subuid;
//   - that root cannot read a 0600 file of the operator;
//   - the worker still pushes a veth into a netns owned by the child.
//
// runc is not exercised here (it needs an image); the holder's overlay mount
// and the nsenter entry are the exact namespaces runc is entered into.
func TestChildUserns_ServiceRootIsASubuid(t *testing.T) {
	if _, ok := lookupSubidTools(); !ok {
		t.Skip("newuidmap/newgidmap unavailable: child userns needs a delegated range")
	}
	skipUnlessRootlessNetns(t)

	openExecutableToOthers(t)

	cn, err := NewSpaceNet("child", "10.93.0.0/24")
	if err != nil {
		t.Skipf("NewSpaceNet: %v", err)
	}
	defer cn.Close()

	// The service root is unmapped as the operator: every path down to the
	// overlay must be traversable by "nobody".
	base := t.TempDir()
	t.Cleanup(func() { _ = RemoveAllAsRoot(base) })
	for d := base; strings.HasPrefix(d, os.TempDir()) && d != os.TempDir(); d = filepath.Dir(d) {
		if err := os.Chmod(d, 0o711); err != nil {
			t.Fatal(err)
		}
	}
	lower, upper, work, merged := filepath.Join(base, "lower"), filepath.Join(base, "upper"), filepath.Join(base, "work"), filepath.Join(base, "merged")
	for _, d := range []string{lower, upper, work, merged} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(lower, "hello"), []byte("from lower\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(base, "operator-secret")
	if err := os.WriteFile(secret, []byte("token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	pid, child, err := cn.SpawnOverlayChild(lower, upper, work, merged, 0)
	if err != nil {
		t.Fatalf("SpawnOverlayChild: %v", err)
	}
	space := cn.Identity()
	if child.HostUID == space.HostUID || child.HostUID < space.UIDBase || child.HostUID >= space.UIDBase+space.UIDCount {
		t.Fatalf("child root %d must be a subuid of %+v, not the operator", child.HostUID, space)
	}

	// The worker wrote the child's maps: seen from the host, id 0 is the subuid.
	uidMap, err := os.ReadFile(fmt.Sprintf("/proc/%d/uid_map", pid))
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("0 %d 1", child.HostUID); !strings.Contains(squash(string(uidMap)), want) {
		t.Fatalf("child uid_map = %q, want a line %q", squash(string(uidMap)), want)
	}
	waitOverlayMounted(t, pid, merged)

	enter := func(args ...string) (string, error) {
		argv := append(ociproc.EnterArgs(pid, true), "--")
		argv = append(argv, args...)
		out, err := exec.Command("nsenter", argv...).CombinedOutput()
		return string(out), err
	}
	if flags := ociproc.EnterArgs(pid, true); !strings.Contains(strings.Join(flags, " "), "-S 0 -G 0") {
		t.Fatalf("EnterArgs must become the child's root, got %v", flags)
	}

	out, err := enter("sh", "-c", "id -u; cat "+merged+"/hello; touch "+merged+"/made")
	if err != nil {
		t.Fatalf("enter child as root: %v: %s", err, out)
	}
	if !strings.HasPrefix(out, "0\nfrom lower\n") {
		t.Fatalf("inside the child: %q", out)
	}
	st, err := os.Stat(filepath.Join(upper, "made"))
	if err != nil {
		t.Fatalf("copy-up landed nowhere: %v", err)
	}
	if got := int(st.Sys().(*syscall.Stat_t).Uid); got != child.HostUID {
		t.Fatalf("file made by the service root is owned by %d on the host, want the subuid %d", got, child.HostUID)
	}

	out, err = enter("cat", secret)
	if err == nil {
		t.Fatalf("the service root read the operator's 0600 file: %q", out)
	}
	if !strings.Contains(out, "Permission denied") {
		t.Fatalf("unexpected failure reading the secret: %v: %s", err, out)
	}

	// A netns created by the child's root, as runc would: the worker, one
	// namespace up, must still be able to push a veth into it.
	pidFile := filepath.Join(merged, "netpid")
	argv := append(ociproc.EnterArgs(pid, true), "--", "unshare", "-n", "sh", "-c", "echo $$ > "+pidFile+"; exec sleep 60")
	svc := exec.Command("nsenter", argv...)
	if err := svc.Start(); err != nil {
		t.Fatal(err)
	}
	netPID := waitPIDFile(t, filepath.Join(upper, "netpid"))
	// The sleep runs as the child's root, a uid this test may not signal: kill
	// it from inside that namespace, where the test holds the capability.
	defer func() {
		_ = exec.Command("nsenter", "-t", strconv.Itoa(pid), "-U", "-S", "0", "-G", "0", "--", "kill", "-9", strconv.Itoa(netPID)).Run()
		_ = svc.Wait()
	}()

	ip, err := cn.Attach("svc", netPID, "eth0")
	if err != nil {
		t.Fatalf("attach a veth into the child's netns: %v", err)
	}
	link, err := exec.Command("nsenter", "-t", strconv.Itoa(netPID), "-U", "-n", "-S", "0", "-G", "0", "--", "ip", "-o", "-4", "addr", "show", "eth0").CombinedOutput()
	if err != nil || !strings.Contains(string(link), ip) {
		t.Fatalf("eth0 in the child's netns: %v: %s (want %s)", err, link, ip)
	}
}

func squash(s string) string { return strings.Join(strings.Fields(s), " ") }

func skipUnlessRootlessNetns(t *testing.T) {
	t.Helper()
	if out, err := exec.Command("unshare", "-Urn", "true").CombinedOutput(); err != nil {
		t.Skipf("rootless user+net namespaces unavailable: %v: %s", err, out)
	}
}

func waitOverlayMounted(t *testing.T, pid int, merged string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		if mi, err := os.ReadFile(fmt.Sprintf("/proc/%d/mountinfo", pid)); err == nil && strings.Contains(string(mi), " "+merged+" ") && strings.Contains(string(mi), "overlay") {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("holder %d never mounted the overlay at %s", pid, merged)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func waitPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no pid written at %s", path)
	return 0
}

// openExecutableToOthers makes the test binary reachable by an id the operator
// is not: `go test` builds it under a 0700 temp dir, and the child's root is
// unmapped as the operator. A real host binary lives in a world-traversable
// directory already.
func openExecutableToOthers(t *testing.T) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for d := filepath.Dir(exe); strings.HasPrefix(d, os.TempDir()) && d != os.TempDir(); d = filepath.Dir(d) {
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(exe, 0o755); err != nil {
		t.Fatal(err)
	}
}
