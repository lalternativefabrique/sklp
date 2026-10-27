//go:build linux

package ociproc

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestIdentityHostUIDOf(t *testing.T) {
	id := Identity{HostUID: 1000, UIDBase: 100000, UIDCount: 65536}
	for in, want := range map[int]int{0: 1000, -1: 1000, 1: 100000, 999: 100998, 65536: 165535} {
		if got, ok := id.HostUIDOf(in); !ok || got != want {
			t.Errorf("HostUIDOf(%d) = %d,%v want %d", in, got, ok, want)
		}
	}
	// One past the range is unmapped: in a child userns that id would be the
	// child's own root, in a single-id identity anything above 0 is nobody.
	if _, ok := id.HostUIDOf(65537); ok {
		t.Error("HostUIDOf(65537) must be unmapped")
	}
	if _, ok := SingleIdentity(1000, 1000).HostUIDOf(1); ok {
		t.Error("single identity maps nothing above 0")
	}
}

func TestIdentityMappings(t *testing.T) {
	id := Identity{HostUID: 1000, HostGID: 1000, UIDBase: 100000, GIDBase: 200000, UIDCount: 65536, GIDCount: 65536}
	uid, gid := id.Mappings()
	wantUID, wantGID := IDMappingsWithSubIDs(1000, 1000, 100000, 65536, 200000, 65536)
	if !reflect.DeepEqual(uid, wantUID) || !reflect.DeepEqual(gid, wantGID) {
		t.Fatalf("range mappings differ from IDMappingsWithSubIDs")
	}
	uid, gid = SingleIdentity(1000, 1000).Mappings()
	if len(uid) != 1 || len(gid) != 1 || uid[0]["hostID"] != 1000 {
		t.Fatalf("single identity mappings = %v %v", uid, gid)
	}
}

func TestRootHostUID(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]struct {
		content string
		want    int
		ok      bool
	}{
		"shared":  {"         0       1000          1\n         1     100000      65536\n", 1000, true},
		"child":   {"         0     165535          1\n         1     100000      65535\n", 165535, true},
		"ranged":  {"         0     100000      65536\n", 100000, true},
		"norootv": {"         1     100000      65536\n", 0, false},
		"empty":   {"", 0, false},
	}
	for name, tc := range cases {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(tc.content), 0o600); err != nil {
			t.Fatal(err)
		}
		got, ok := rootHostUID(p)
		if ok != tc.ok || got != tc.want {
			t.Errorf("%s: rootHostUID = %d,%v want %d,%v", name, got, ok, tc.want, tc.ok)
		}
	}
	if _, ok := rootHostUID(filepath.Join(dir, "missing")); ok {
		t.Error("missing map reported ok")
	}
}

func TestEnterArgs(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	const caller = 1000
	cases := []struct {
		name string
		path string
		want string
	}{
		{"worker userns maps the caller as root", write("shared", "0 1000 1\n1 100000 65536\n"), "--preserve-credentials"},
		{"child userns maps a subuid as root", write("child", "0 165535 1\n1 100000 65535\n"), "-S"},
		{"caller mapped but not as root", write("shifted", "0 0 1\n1 1 65536\n"), "-S"},
		{"root inside its own userns", write("ciroot", "0 0 1\n1 1 65536\n"), "--preserve-credentials"},
		{"unreadable map keeps credentials", filepath.Join(dir, "missing"), "--preserve-credentials"},
	}
	for _, tc := range cases {
		callerUID := caller
		if tc.name == "root inside its own userns" {
			callerUID = 0
		}
		got := enterArgs(42, tc.name == cases[0].name, tc.path, callerUID)
		if got[len(got)-1] != tc.want && got[len(got)-4] != tc.want {
			t.Errorf("%s: enterArgs = %v, want it to end with %s", tc.name, got, tc.want)
		}
	}
	if got := enterArgs(42, true, cases[0].path, caller); !reflect.DeepEqual(got, []string{"-t", "42", "-U", "--mount", "--preserve-credentials"}) {
		t.Fatalf("mount flag placement: %v", got)
	}
	if got := enterArgs(42, false, cases[1].path, caller); !reflect.DeepEqual(got, []string{"-t", "42", "-U", "-S", "0", "-G", "0"}) {
		t.Fatalf("child enter: %v", got)
	}
}
