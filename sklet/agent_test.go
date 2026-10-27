package sklet

import (
	"os"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/internal/config"
	"github.com/lalternativefabrique/sklp/spacemanager"
	"gopkg.in/yaml.v3"
)

// readConfigFile decodes a config file written by writeNodeConfig (an arbitrary
// path, not the fixed config.Load location).
func readConfigFile(path string) (*config.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c config.Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// TestWriteNodeConfig_AdvertiseFillsServer: with --advertise, the generated
// node config has a complete `default` deploy context (token + fingerprint +
// space) and a server URL combining the advertised host and the listen port.
func TestWriteNodeConfig_AdvertiseFillsServer(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	path, err := writeNodeConfig("production", "1.2.3.4", ":6464")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := readConfigFile(path)
	if err != nil {
		t.Fatalf("reload generated config: %v", err)
	}
	p, ok := cfg.Contexts["default"]
	if !ok {
		t.Fatal("generated config has no `default` context")
	}
	if p.Server != "https://1.2.3.4:6464" {
		t.Fatalf("server = %q, want https://1.2.3.4:6464 (advertise host + listen port)", p.Server)
	}
	if p.Token == "" || p.CAFingerprint == "" {
		t.Fatalf("token/fingerprint empty: %q / %q", p.Token, p.CAFingerprint)
	}
	if p.Space != "production" {
		t.Fatalf("space = %q, want production", p.Space)
	}
}

// TestWriteNodeConfig_NoAdvertisePlaceholder: without --advertise the context
// is still complete but server carries a REPLACE_WITH_VM_PUBLIC_IP placeholder
// (with the listen port), and the file has a TODO header telling the operator
// to fill it in.
func TestWriteNodeConfig_NoAdvertisePlaceholder(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	path, err := writeNodeConfig("production", "", ":6464")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := readConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if s := cfg.Contexts["default"].Server; s != "https://REPLACE_WITH_VM_PUBLIC_IP:6464" {
		t.Fatalf("server = %q, want the REPLACE_WITH_VM_PUBLIC_IP:6464 placeholder", s)
	}
	if cfg.Contexts["default"].Token == "" {
		t.Fatal("token should still be set")
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "REPLACE_WITH_VM_PUBLIC_IP") || !strings.Contains(string(raw), "# ") {
		t.Fatal("file should carry a header comment guiding the operator")
	}
}

// TestAgentGatewayDriverAlwaysWired: an EMPTY node (meta with no routes) must
// still get a gateway driver, so the front door is up before the first routed
// deploy — otherwise SetRoutes no-ops (a.gateway nil) and traefik 404s. This is
// the regression the empty-agent mode introduced.
func TestAgentGatewayDriverAlwaysWired(t *testing.T) {
	// Empty meta: no routes at all (the fresh-node case).
	empty := spacemanager.SpaceMeta{Name: "production"}
	if d := agentGatewayDriver(empty); d == "" {
		t.Fatal("empty node got no gateway driver; the front door would never stand up")
	}
	// A meta that already declares routes keeps its resolved driver.
	routed := spacemanager.SpaceMeta{
		Name:   "production",
		Routes: []spacemanager.ResolvedRoute{{Service: "web", Host: "shop.fr"}},
	}
	if d := agentGatewayDriver(routed); d == "" {
		t.Fatal("routed node got no gateway driver")
	}
}

func TestPrivateOriginListen(t *testing.T) {
	cases := []struct {
		listen, via, endpoint string
		want                  string
		ok                    bool
	}{
		{":6464", "nodes.example", "10.200.0.10:51820", "10.200.0.10:6464", true},
		{"0.0.0.0:6464", "nodes.example", "10.200.0.10:51820", "10.200.0.10:6464", true},
		{"[::]:6464", "nodes.example", "10.200.0.10:51820", "10.200.0.10:6464", true},
		{"127.0.0.1:6464", "nodes.example", "10.200.0.10:51820", "", false},
		{":6464", "", "10.200.0.10:51820", "", false},
		{":6464", "nodes.example", "", "", false},
		{":6464", "nodes.example", "not-an-ip:51820", "", false},
	}
	for _, c := range cases {
		got, ok := privateOriginListen(c.listen, c.via, c.endpoint)
		if ok != c.ok || got != c.want {
			t.Errorf("privateOriginListen(%q,%q,%q) = %q,%v want %q,%v", c.listen, c.via, c.endpoint, got, ok, c.want, c.ok)
		}
	}
}
