package redirect

import "strings"

import "testing"

// TestRenderInstall asserts the nft script is atomic (flush before add),
// exempts wg0 mesh traffic before any redirect, and carries a prerouting +
// output redirect for every mapping.
func TestRenderInstall(t *testing.T) {
	got := renderInstall(DefaultMappings())

	for _, sub := range []string{
		"add table ip " + redirectTable,
		"flush table ip " + redirectTable, // idempotent re-install
		"chain ip " + redirectTable + " prerouting { type nat hook prerouting priority -100 ; }",
		"chain ip " + redirectTable + " output { type nat hook output priority -100 ; }",
		`prerouting iifname "wg0" accept`,
		"prerouting tcp dport 80 redirect to :9080",
		"prerouting tcp dport 443 redirect to :9443",
		"output ip daddr 127.0.0.1 tcp dport 80 redirect to :9080",
		"output ip daddr 127.0.0.1 tcp dport 443 redirect to :9443",
	} {
		if !strings.Contains(got, sub) {
			t.Errorf("install script missing %q\n%s", sub, got)
		}
	}

	// flush must precede the first add rule (atomic re-install).
	if strings.Index(got, "flush table") > strings.Index(got, "add rule") {
		t.Errorf("flush must come before add rule:\n%s", got)
	}

	// wg0 exemption must precede every redirect, or mesh traffic still gets
	// NAT'ed into the gateway mux.
	if strings.Index(got, `iifname "wg0" accept`) > strings.Index(got, "redirect to :9080") {
		t.Errorf("wg0 accept must come before the redirect rules:\n%s", got)
	}
}

// TestDefaultMappings pins the front-door contract: 80->9080, 443->9443.
func TestDefaultMappings(t *testing.T) {
	want := []Mapping{{80, 9080}, {443, 9443}}
	got := DefaultMappings()
	if len(got) != len(want) {
		t.Fatalf("got %d mappings, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("mapping %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
