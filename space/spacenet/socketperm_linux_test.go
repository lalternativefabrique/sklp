//go:build linux

package spacenet

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The attach and ingress sockets are unauthenticated command channels on
// derivable paths: ingress splices a caller-named service:port from inside the
// space netns, attach mutates that netns. Their only access control is the mode
// of the directory holding them, so these tests pin that contract.

func TestSockDirIsPrivateAndUnderRuntimeRoot(t *testing.T) {
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	dir := sockDir()
	if dir == "" {
		t.Fatal("sockDir() returned empty for a usable runtime dir")
	}
	if !strings.HasPrefix(dir, runtimeDir) {
		t.Fatalf("sockDir() = %q, want it under XDG_RUNTIME_DIR %q", dir, runtimeDir)
	}

	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat %q: %v", dir, err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Fatalf("sockDir() mode = %#o, want 0700 (any looser lets another local user reach the space netns)", perm)
	}
}

func TestSockDirTightensAPreexistingLooseDir(t *testing.T) {
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	loose := filepath.Join(runtimeDir, "sklp")
	if err := os.MkdirAll(loose, 0o755); err != nil {
		t.Fatalf("pre-create loose dir: %v", err)
	}

	if dir := sockDir(); dir == "" {
		t.Fatal("sockDir() returned empty for a pre-existing dir")
	}

	fi, err := os.Stat(loose)
	if err != nil {
		t.Fatalf("stat %q: %v", loose, err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Fatalf("pre-existing dir left at %#o, want it forced to 0700", perm)
	}
}

func TestSocketPathsAreNotInWorldWritableTmp(t *testing.T) {
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	for _, tc := range []struct {
		name string
		path string
	}{
		{"ingress", ingressSockPath("prod")},
		{"attach", attachSockPath("prod")},
	} {
		if tc.path == "" {
			t.Fatalf("%s socket path is empty", tc.name)
		}
		if !strings.HasPrefix(tc.path, runtimeDir) {
			t.Errorf("%s socket at %q, want it under the 0700 runtime dir %q", tc.name, tc.path, runtimeDir)
		}
		// A unix socket path is capped near 108 bytes by the kernel.
		if len(tc.path) > 100 {
			t.Errorf("%s socket path is %d bytes, too close to the 108-byte sun_path cap", tc.name, len(tc.path))
		}
	}
}

// checkPeerUID must ACCEPT a same-uid peer, otherwise the hardening would break
// the legitimate mux -> worker channel.
func TestCheckPeerUIDAcceptsSameUser(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "peer.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	accepted := make(chan error, 1)
	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			accepted <- aerr
			return
		}
		defer conn.Close()
		accepted <- checkPeerUID(conn.(*net.UnixConn))
	}()

	client, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	if err := <-accepted; err != nil {
		t.Fatalf("checkPeerUID rejected our own uid: %v", err)
	}
}
