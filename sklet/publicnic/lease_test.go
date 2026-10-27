package publicnic

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleLease = `# This is private data. Do not parse.
ADDRESS=198.51.100.34
NETMASK=255.255.255.255
ROUTER=198.51.100.1 198.51.100.2
SERVER_ADDRESS=198.51.100.1
LIFETIME=86400
`

func TestParseLease(t *testing.T) {
	cases := []struct {
		name, in, addr, router, err string
	}{
		{name: "networkd lease", in: sampleLease, addr: "198.51.100.34", router: "198.51.100.1"},
		{name: "no router", in: "ADDRESS=198.51.100.34\n", err: "no ROUTER"},
		{name: "no address", in: "ROUTER=198.51.100.1\n", err: "no ADDRESS"},
		{name: "garbage router", in: "ADDRESS=1.2.3.4\nROUTER=nope\n", err: "no ROUTER"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l, err := ParseLease(strings.NewReader(c.in))
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("err = %v, want %q", err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if l.Address.String() != c.addr || l.Router.String() != c.router {
				t.Fatalf("got %s via %s", l.Address, l.Router)
			}
		})
	}
}

func TestReadLeaseByIfindex(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "3"), []byte(sampleLease), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLease(dir, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLease(dir, 4); !os.IsNotExist(err) {
		t.Fatalf("missing lease err = %v", err)
	}
}
