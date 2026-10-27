package embed

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExtractRuncSlirpFromEmbedded(t *testing.T) {
	cache := t.TempDir()

	rp, err := ExtractRunc(cache)
	if err != nil {
		t.Fatalf("ExtractRunc: %v", err)
	}
	sp, err := ExtractSlirp(cache)
	if err != nil {
		t.Fatalf("ExtractSlirp: %v", err)
	}

	for _, c := range []struct {
		name, path string
	}{
		{"runc", rp},
		{"slirp4netns", sp},
	} {
		want := filepath.Join(cache, "runtime", "bin", c.name)
		if c.path != want {
			t.Errorf("%s path = %q, want %q", c.name, c.path, want)
		}
		info, err := os.Stat(c.path)
		if err != nil {
			t.Fatalf("%s stat: %v", c.name, err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s not executable: %v", c.name, info.Mode())
		}
		if info.Size() == 0 {
			t.Errorf("%s is empty", c.name)
		}
	}
}
