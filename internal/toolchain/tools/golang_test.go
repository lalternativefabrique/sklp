package tools

import "testing"

// goFixtureManifest is a trimmed JSON sample from go.dev/dl/?mode=json
// covering exactly two releases, two os/arch combos each. Lets us
// pin the parser without making the test depend on the live manifest.
const goFixtureManifest = `[
  {
    "version": "go1.25.0",
    "files": [
      { "filename": "go1.25.0.linux-amd64.tar.gz", "os": "linux",  "arch": "amd64", "sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "kind": "archive" },
      { "filename": "go1.25.0.darwin-arm64.tar.gz", "os": "darwin", "arch": "arm64", "sha256": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "kind": "archive" },
      { "filename": "go1.25.0.linux-amd64.tar.gz.asc", "os": "linux", "arch": "amd64", "kind": "source" }
    ]
  },
  {
    "version": "go1.24.0",
    "files": [
      { "filename": "go1.24.0.linux-amd64.tar.gz", "os": "linux", "arch": "amd64", "sha256": "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", "kind": "archive" }
    ]
  }
]`

func TestGoManifestSHA(t *testing.T) {
	cases := []struct {
		version, os, arch string
		want              string
		wantErr           bool
	}{
		{"1.25.0", "linux", "amd64", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", false},
		{"1.25.0", "darwin", "arm64", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", false},
		{"1.24.0", "linux", "amd64", "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", false},
		// version exists, os/arch combo doesn't
		{"1.25.0", "linux", "arm64", "", true},
		// completely unknown version
		{"0.0.1", "linux", "amd64", "", true},
		// asc files have kind=source — we must ignore them
		{"1.25.0", "linux", "amd64-source", "", true},
	}
	for _, c := range cases {
		got, err := goManifestSHA(c.version, c.os, c.arch, goFixtureManifest)
		if c.wantErr {
			if err == nil {
				t.Errorf("(%s, %s, %s): expected error, got %q", c.version, c.os, c.arch, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("(%s, %s, %s): unexpected error: %v", c.version, c.os, c.arch, err)
			continue
		}
		if got != c.want {
			t.Errorf("(%s, %s, %s): got %s, want %s", c.version, c.os, c.arch, got, c.want)
		}
	}
}

func TestGoURL(t *testing.T) {
	g := &golang{}
	url, err := g.URL("1.25.0", "linux", "amd64")
	if err != nil {
		t.Fatalf("url: %v", err)
	}
	want := "https://go.dev/dl/go1.25.0.linux-amd64.tar.gz"
	if url != want {
		t.Errorf("url = %s, want %s", url, want)
	}
}
