//go:build linux

package spacenet

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestNewuidmapArgs checks the exact triplets passed to newuidmap/newgidmap:
// one line maps container id 0 -> host id (size 1), the next maps 1..65536 ->
// the subuid range base. newuidmap argv is "<pid> <containerID> <hostID> <size>".
func TestNewuidmapArgs(t *testing.T) {
	cases := []struct {
		name      string
		pid, host int
		uidWant   []string
		gidWant   []string
	}{
		{
			name: "typical uid 1000",
			pid:  4242, host: 1000,
			uidWant: []string{"4242", "0", "1000", "1", "1", "100000", "65536"},
			gidWant: []string{"4242", "0", "1000", "1", "1", "100000", "65536"},
		},
		{
			name: "root host id 0",
			pid:  7, host: 0,
			uidWant: []string{"7", "0", "0", "1", "1", "100000", "65536"},
			gidWant: []string{"7", "0", "0", "1", "1", "100000", "65536"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := newuidmapArgs(tc.pid, tc.host, 100000, 65536); !reflect.DeepEqual(got, tc.uidWant) {
				t.Errorf("newuidmapArgs(%d,%d) = %v, want %v", tc.pid, tc.host, got, tc.uidWant)
			}
			if got := newgidmapArgs(tc.pid, tc.host, 100000, 65536); !reflect.DeepEqual(got, tc.gidWant) {
				t.Errorf("newgidmapArgs(%d,%d) = %v, want %v", tc.pid, tc.host, got, tc.gidWant)
			}
		})
	}

	// A non-100000 base (the 2nd cloud-init user lands at 165536) must flow
	// through verbatim — the regression this fix addresses.
	if got := newuidmapArgs(4242, 1000, 165536, 65536); !reflect.DeepEqual(got,
		[]string{"4242", "0", "1000", "1", "1", "165536", "65536"}) {
		t.Errorf("newuidmapArgs with base 165536 = %v", got)
	}
}

// TestLookupSubidTools_FallbackWhenMissing forces an empty PATH so neither
// helper resolves: lookupSubidTools must report ok=false, the signal the
// caller uses to fall back to the single-id mapping (no range, log warning).
func TestLookupSubidTools_FallbackWhenMissing(t *testing.T) {
	t.Setenv("PATH", "") // no directory to search: LookPath fails for both
	if _, ok := lookupSubidTools(); ok {
		t.Fatal("lookupSubidTools ok=true with empty PATH, want false (fallback)")
	}
}

// TestLookupSubidTools_FoundWhenPresent stages fake newuidmap/newgidmap on a
// scratch PATH and asserts both resolve to those paths.
func TestLookupSubidTools_FoundWhenPresent(t *testing.T) {
	dir := t.TempDir()
	uid := filepath.Join(dir, "newuidmap")
	gid := filepath.Join(dir, "newgidmap")
	for _, p := range []string{uid, gid} {
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	tools, ok := lookupSubidTools()
	if _, _, _, rerr := resolveSubidRange(os.Getuid()); rerr != nil {
		// No /etc/subuid entry for this user (root in a container): the helpers
		// are useless and the caller must take the single-id path.
		if ok {
			t.Fatal("lookupSubidTools ok=true without a subuid range, want false")
		}
		return
	}
	if !ok {
		t.Fatal("lookupSubidTools ok=false with helpers on PATH and a subuid range, want true")
	}
	if tools.newuidmap != uid || tools.newgidmap != gid {
		t.Errorf("resolved {%s,%s}, want {%s,%s}", tools.newuidmap, tools.newgidmap, uid, gid)
	}
}
