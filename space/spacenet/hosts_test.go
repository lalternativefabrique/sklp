//go:build linux

package spacenet

import (
	"os"
	"strings"
	"testing"
)

// TestHostsPath_Content asserts the generated /etc/hosts carries the minimal
// loopback entries (the load-bearing fix for images that resolve localhost /
// 0.0.0.0 in a netns with no /etc/hosts) plus the gateway -> hostname line,
// and that repeated calls return the same cached file.
func TestHostsPath_Content(t *testing.T) {
	a, err := newIPAM("10.88.0.0/24")
	if err != nil {
		t.Fatalf("newIPAM: %v", err)
	}
	c := &SpaceNet{name: "demo", ipam: a}

	path, err := c.HostsPath()
	if err != nil {
		t.Fatalf("HostsPath: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read hosts: %v", err)
	}
	content := string(b)
	if !strings.Contains(content, "127.0.0.1\tlocalhost") {
		t.Errorf("hosts missing loopback entry, got:\n%s", content)
	}
	if !strings.Contains(content, "::1\tlocalhost") {
		t.Errorf("hosts missing IPv6 loopback entry, got:\n%s", content)
	}
	if !strings.Contains(content, "127.0.1.1\tsklp") {
		t.Errorf("the hostname must resolve to the container itself, got:\n%s", content)
	}
	if strings.Contains(content, "10.88.0.1") {
		t.Errorf("the hostname must not resolve to the bridge gateway, got:\n%s", content)
	}

	// Cached: a second call returns the identical path, not a new temp file.
	if again, err := c.HostsPath(); err != nil || again != path {
		t.Errorf("HostsPath not cached: %q vs %q (err %v)", again, path, err)
	}
}
