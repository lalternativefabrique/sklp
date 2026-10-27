package space

import (
	"os"
	"testing"
)

// TestParseVolume covers the docker -v form: named volumes land under the
// volumes root, host binds pass through, and malformed entries fail.
func TestParseVolume(t *testing.T) {
	const root = "/cache/dev-volumes"
	cases := []struct {
		entry   string
		want    volumeMount
		wantErr bool
	}{
		{"sklp-dev-pg:/var/lib/postgresql/data",
			volumeMount{hostPath: "/cache/dev-volumes/sklp-dev-pg", destination: "/var/lib/postgresql/data", named: true}, false},
		{"/home/dev/data:/data",
			volumeMount{hostPath: "/home/dev/data", destination: "/data"}, false},
		{"/etc/resolv.conf:/etc/resolv.conf:ro",
			volumeMount{hostPath: "/etc/resolv.conf", destination: "/etc/resolv.conf", readOnly: true}, false},
		{"/home/dev/data:/data:rw",
			volumeMount{hostPath: "/home/dev/data", destination: "/data"}, false},
		{"/x:/y:bogus", volumeMount{}, true}, // unknown mode
		{"noColon", volumeMount{}, true},
		{"name:relative/path", volumeMount{}, true}, // container path not absolute
		{"a/b:/data", volumeMount{}, true},          // named volume with separator
		{"..:/data", volumeMount{}, true},           // would join out of volumesRoot
		{".:/data", volumeMount{}, true},
		{"~:/data", volumeMount{}, true},
		{".hidden:/data", volumeMount{}, true},
		{":/data", volumeMount{}, true}, // empty source
		{"name:", volumeMount{}, true},  // empty destination
	}
	for _, c := range cases {
		got, err := parseVolume(c.entry, root)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseVolume(%q): want error, got %+v", c.entry, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseVolume(%q): unexpected error %v", c.entry, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseVolume(%q) = %+v, want %+v", c.entry, got, c.want)
		}
	}
}

func TestValidateNamedVolume(t *testing.T) {
	ok := []string{"pgdata:/var/lib/postgresql/data", "dumps:/backups:ro", "a.b-c_d:/x:rw"}
	for _, e := range ok {
		if err := ValidateNamedVolume(e); err != nil {
			t.Errorf("ValidateNamedVolume(%q): unexpected error %v", e, err)
		}
	}
	rejected := []string{
		"/home/user/.config/sklp/control:/loot:ro",
		"/:/host",
		"/etc/resolv.conf:/etc/resolv.conf:ro",
		"..:/data",
		"../x:/data",
		"./data:/data",
		"~/data:/data",
		"pgdata:relative",
		"pgdata:/data:bogus",
	}
	for _, e := range rejected {
		if err := ValidateNamedVolume(e); err == nil {
			t.Errorf("ValidateNamedVolume(%q): want error", e)
		}
	}
}

// TestResolveVolumes_CreatesHostDirs proves a named volume's backing dir is
// created (a runc bind-mount needs an existing source), order is preserved, and
// each entry becomes an rw DevMount at its container destination.
func TestResolveVolumes_CreatesHostDirs(t *testing.T) {
	root := t.TempDir()
	got, err := resolveVolumes([]string{"pgdata:/var/lib/postgresql/data", "natsdata:/data"}, root)
	if err != nil {
		t.Fatalf("resolveVolumes: %v", err)
	}
	if len(got) != 2 || got[0].Destination != "/var/lib/postgresql/data" || got[1].Destination != "/data" {
		t.Fatalf("resolveVolumes order/shape wrong: %+v", got)
	}
	for _, m := range got {
		fi, err := os.Stat(m.Source)
		if err != nil || !fi.IsDir() {
			t.Errorf("host dir %q not created (err=%v)", m.Source, err)
			continue
		}
		if perm := fi.Mode().Perm(); perm != 0o700 {
			t.Errorf("host dir %q mode = %o, want 0700", m.Source, perm)
		}
	}
}

func TestAnonImageVolumesCreatesPrivateDirs(t *testing.T) {
	root := t.TempDir()
	got, err := anonImageVolumes("db", map[string]struct{}{"/var/lib/postgresql/data": {}}, nil, root)
	if err != nil {
		t.Fatalf("anonImageVolumes: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("anonImageVolumes returned %d mounts, want 1", len(got))
	}
	fi, err := os.Stat(got[0].Source)
	if err != nil {
		t.Fatalf("stat anon volume: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Fatalf("anon volume mode = %o, want 0700", perm)
	}
}
