package main

import (
	"context"
	"fmt"
	"os"

	"errors"
	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/sklet"
	"github.com/lalternativefabrique/sklp/sklet/publicnic"
	"github.com/lalternativefabrique/sklp/spacemanager"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
)

// agentCommand declares `sklet agent` — the control-plane node mode: a
// supervisor with ZERO services that opens the control API and waits for a
// remote `sklp deploy <stack> --server` to push the first manifest. The sklp
// equivalent of a kubelet that boots empty and waits to be scheduled onto.
func agentCommand() cli.Command {
	return cli.Command{
		Name:  "agent",
		Short: "Run an empty control-plane node: open the control API and wait for a remote deploy",
		Long: "Start a supervisor with no services and expose the control API on --listen, so a\n" +
			"remote `sklp deploy <stack> --server https://host:<port>` can push the first\n" +
			"manifest over the network. The node generates its own cert + token; nothing is\n" +
			"deployed locally. This is the sklp equivalent of a kubelet that boots empty and\n" +
			"waits to be scheduled onto.",
		Flags: []cli.Flag{
			{Name: "listen", Type: cli.StringFlag, StringDefault: ":6464", Usage: "TCP address to expose the control API on over TLS + a Bearer token (must NOT be the :9443/:9080 front door)"},
			{Name: "space", Type: cli.StringFlag, Usage: "ignored; kept so agent units rendered before its removal still start"},
			{Name: "advertise", Type: cli.StringFlag, Usage: "public host/IP written into the generated node config's server field (e.g. 203.0.113.10); empty leaves server blank for the operator to fill in"},
			{Name: "gateway-mode", Type: cli.StringFlag, Usage: "REQUIRED: front door for this node: per-space (native mux on :9080/:9443) or singleton (legacy traefik). No default — the mode is a node-level decision, never inferred from a deploy.yaml"},
			{Name: "mesh-authority-url", Type: cli.StringFlag, Usage: "meshauthority base URL (e.g. https://mesh.internal:4300); empty disables the cross-VM WireGuard mesh (ADR-006)"},
			{Name: "mesh-authority-url-internal", Type: cli.StringFlag, Usage: "ignored; kept so agent units rendered before its removal still start"},
			{Name: "mesh-endpoint", Type: cli.StringFlag, Usage: "this node's own dialable IP:port for the WireGuard mesh (its private cloud interface address + the mesh listen port, e.g. 10.200.0.10:51820); required when --mesh-authority-url is set"},
			{Name: "node-relay-domain", Type: cli.StringFlag, Usage: "on a front: relay any control API connection whose TLS SNI is <node-id>.<domain> to that mesh peer's own control API over the private network, TLS untouched (ADR-019). Needs the mesh and a wildcard DNS record *.<domain> on this node's public IP"},
			{Name: "advertise-via", Type: cli.StringFlag, Usage: "on a private origin: advertise https://<own mesh node-id>.<domain>:<port> as this node's server, the name the front's --node-relay-domain answers for; ignored when --advertise is set"},
			{Name: "public-nic-interface", Type: cli.StringFlag, Usage: "public NIC whose policy routing and SMTP egress the node keeps in place (node.yaml public_nic); empty disables it"},
			{Name: "public-nic-source", Type: cli.StringFlag, Usage: "source address routed out of the public NIC (the Additional IP); empty uses the NIC's DHCP address"},
			{Name: "public-nic-table", Type: cli.IntFlag, IntDefault: publicnic.DefaultTable, Usage: "routing table holding the public NIC's default route"},
			{Name: "public-nic-mark", Type: cli.IntFlag, IntDefault: publicnic.DefaultMark, Usage: "fwmark steering outbound SMTP into the public NIC's table"},
			{Name: "mesh-cert-fingerprint", Type: cli.StringFlag, Usage: "SHA-256 fingerprint (hex, colons optional) of meshauthority's self-signed certificate, printed by meshauthority at startup. Required to reach an https:// authority: there is no CA to validate it against, so the node pins this instead. Empty for a plain http:// authority."},
		},
		Run: func(ctx context.Context, args cli.Args) int {
			listen := args.String("listen")
			advertise := args.String("advertise")
			gatewayMode := args.String("gateway-mode")
			meshAuthorityURL := args.String("mesh-authority-url")
			// The meshauthority bearer token is a secret: read from the
			// environment only, never a CLI flag — a flag value is visible to
			// every local user via /proc/<pid>/cmdline for the life of the
			// process (the same class of leak fixed for runc exec, PR #763).
			meshToken := os.Getenv("SKLP_MESH_TOKEN")
			// Space workers are re-execs of this binary and inherit its
			// environment; none of them talks to meshauthority.
			os.Unsetenv("SKLP_MESH_TOKEN")
			meshEndpoint := args.String("mesh-endpoint")
			if meshAuthorityURL != "" && (meshToken == "" || meshEndpoint == "") {
				fmt.Fprintln(os.Stderr, "sklet agent: SKLP_MESH_TOKEN (env) and --mesh-endpoint are required when --mesh-authority-url is set")
				return 2
			}
			if listen == "" {
				fmt.Fprintln(os.Stderr, "sklet agent: --listen is required (e.g. :6464)")
				return 2
			}
			if gatewayMode == "" {
				fmt.Fprintln(os.Stderr, "sklet agent: --gateway-mode is required (per-space or singleton); the node's front door is never inferred from a deploy.yaml")
				return 2
			}
			if err := spacemanager.ValidateGatewayMode(gatewayMode); err != nil {
				fmt.Fprintln(os.Stderr, "sklet agent:", err)
				return 2
			}
			meshOpts := sklet.MeshOptions{
				AuthorityURL:    meshAuthorityURL,
				Token:           meshToken,
				Endpoint:        meshEndpoint,
				CertFingerprint: args.String("mesh-cert-fingerprint"),
				RelayDomain:     args.String("node-relay-domain"),
				AdvertiseVia:    args.String("advertise-via"),
			}
			var publicNIC *publicnic.Config
			if iface := args.String("public-nic-interface"); iface != "" {
				publicNIC = &publicnic.Config{
					Interface: iface,
					Source:    args.String("public-nic-source"),
					Table:     args.Int("public-nic-table"),
					Mark:      uint32(args.Int("public-nic-mark")),
				}
				if err := publicNIC.Validate(); err != nil {
					fmt.Fprintln(os.Stderr, "sklet agent:", err)
					return 2
				}
			}
			if err := sklet.RunAgent(listen, advertise, gatewayMode, meshOpts, publicNIC); err != nil {
				fmt.Fprintln(os.Stderr, "sklet agent:", err)
				if errors.Is(err, spacemanager.ErrConfig) {
					// The unit carries this code as RestartPreventExitStatus, so the
					// node settles in `failed` with the reason readable instead of
					// relaunching every RestartSec against a human-only fix.
					return control.ExitConfigError
				}
				return 1
			}
			return 0
		},
	}
}
