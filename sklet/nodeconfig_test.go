package sklet

import (
	"strings"
	"testing"
)

const nodeYAML = `
listen: ":6464"
gateway_mode: per-space
advertise_via: nodes.skalpai.dev
data_root: /data/sklp
acme_email: ops@example.com
mesh:
  authority_url: https://10.73.0.10:4300
  endpoint: 10.73.0.21:51820
  cert_fingerprint: ab:cd:ef:01:23:45:67:89:ab:cd:ef:01:23:45:67:89:ab:cd:ef:01:23:45:67:89:ab:cd:ef:01:23:45:67:89
policy:
  trusted_registries: [rg.fr-par.scw.cloud, docker.io]
  require_image_digest: true
`

func TestRenderAgentUnit(t *testing.T) {
	cfg, err := ParseNodeConfig([]byte(nodeYAML))
	if err != nil {
		t.Fatal(err)
	}
	unit := cfg.RenderAgentUnit("/home/user/.local/bin/sklet", "/home/user/.config/sklp/node.env")
	for _, want := range []string{
		"ExecStart=/home/user/.local/bin/sklet agent --listen :6464 --gateway-mode per-space --advertise-via nodes.skalpai.dev --mesh-authority-url https://10.73.0.10:4300 --mesh-endpoint 10.73.0.21:51820 --mesh-cert-fingerprint ab:cd:ef:01:23:45:67:89:ab:cd:ef:01:23:45:67:89:ab:cd:ef:01:23:45:67:89:ab:cd:ef:01:23:45:67:89\n",
		"Environment=SKLP_DATA_ROOT=/data/sklp\n",
		"Environment=SKLP_GATEWAY_ACME_EMAIL=ops@example.com\n",
		"Environment=SKLP_TRUSTED_REGISTRIES=rg.fr-par.scw.cloud,docker.io\n",
		"Environment=SKLP_REQUIRE_IMAGE_DIGEST=1\n",
		"EnvironmentFile=-/home/user/.config/sklp/node.env\n",
		"Restart=always\n",
		"RestartPreventExitStatus=78\n",
		"WantedBy=default.target\n",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
	if strings.Contains(unit, "SKLP_MESH_TOKEN") {
		t.Error("a secret leaked into the unit")
	}
}

func TestParseNodeConfig_Rejects(t *testing.T) {
	for name, y := range map[string]string{
		"unknown key":      "listen: \":6464\"\ngateway_mode: per-space\nbogus: 1\n",
		"no listen":        "gateway_mode: per-space\n",
		"bad mode":         "listen: \":6464\"\ngateway_mode: nope\n",
		"mesh no endpoint": "listen: \":6464\"\ngateway_mode: per-space\nmesh:\n  authority_url: http://a\n",
		"mesh over http":   "listen: \":6464\"\ngateway_mode: per-space\nmesh:\n  authority_url: http://10.73.0.10:4300\n  endpoint: 10.73.0.21:51820\n",
		"both advertise":   "listen: \":6464\"\ngateway_mode: per-space\nadvertise: 1.2.3.4\nadvertise_via: x\n",
		"injection":        "listen: \":6464\"\ngateway_mode: per-space\nadvertise: \"1.2.3.4 --evil\"\n",
	} {
		if _, err := ParseNodeConfig([]byte(y)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPublicNICAgentArgs(t *testing.T) {
	base := "listen: \":6464\"\ngateway_mode: per-space\n"
	cases := []struct {
		name, yaml, want string
	}{
		{"absent", base, ""},
		{"defaults", base + "public_nic: {}\n", "--public-nic-interface pub0 --public-nic-table 100 --public-nic-mark 25"},
		{"additional ip, hex mark", base + "public_nic:\n  source: 203.0.113.213\n  mark: 0x19\n", "--public-nic-interface pub0 --public-nic-source 203.0.113.213 --public-nic-table 100 --public-nic-mark 25"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := ParseNodeConfig([]byte(c.yaml))
			if err != nil {
				t.Fatal(err)
			}
			args := strings.Join(cfg.AgentArgs(), " ")
			if c.want == "" && strings.Contains(args, "public-nic") {
				t.Fatalf("absent block rendered %q", args)
			}
			if !strings.Contains(args, c.want) {
				t.Fatalf("args %q lack %q", args, c.want)
			}
		})
	}
	for _, bad := range []string{"public_nic:\n  source: nope\n", "public_nic:\n  table: 254\n", "public_nic:\n  interfac: pub0\n"} {
		if _, err := ParseNodeConfig([]byte(base + bad)); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}
