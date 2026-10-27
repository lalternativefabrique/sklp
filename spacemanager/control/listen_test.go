package control

import (
	"os"
	"testing"
)

// TestListenAddrRoundTrip: SaveListenAddr persists the addr 0600 and LoadListenAddr
// reads it back; a missing file is ("", nil) (socket-only space); an empty addr
// is a no-op (no file written).
func TestListenAddrRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// Missing => "".
	if addr, err := LoadListenAddr("production"); err != nil || addr != "" {
		t.Fatalf("LoadListenAddr (missing) = (%q,%v), want (\"\",nil)", addr, err)
	}

	// Empty addr is a no-op: still no file.
	if err := SaveListenAddr("production", ""); err != nil {
		t.Fatalf("SaveListenAddr(empty): %v", err)
	}
	if _, err := os.Stat(ListenAddrPath("production")); !os.IsNotExist(err) {
		t.Fatalf("empty SaveListenAddr should not create a file (err=%v)", err)
	}

	// Persist + read back.
	if err := SaveListenAddr("production", ":9443"); err != nil {
		t.Fatalf("SaveListenAddr: %v", err)
	}
	info, err := os.Stat(ListenAddrPath("production"))
	if err != nil {
		t.Fatalf("stat listen: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("listen perms = %o, want 600", perm)
	}
	addr, err := LoadListenAddr("production")
	if err != nil || addr != ":9443" {
		t.Fatalf("LoadListenAddr = (%q,%v), want (:9443,nil)", addr, err)
	}
}
