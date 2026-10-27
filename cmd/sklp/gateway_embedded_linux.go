// gateway_embedded.go is the real `sklp gateway` implementation, compiled only
// with -tags embedded_runtime (the same gate deploy/runtime use). The non-gated
// gateway_stub.go gives an actionable error for a binary built without it,
// mirroring deploy_stub.go.
//
// `sklp gateway` manages the host-level ingress singleton (the shared :443/:80
// front door across all spaces). It selects the gateway DRIVER from deploy.yaml
// (gateway.driver, default traefik) and depends only on the spacemanager.GatewayDriver
// interface — never on a traefik type — keeping sklp gateway-agnostic (the
// driver name is the only traefik-aware string, read here at the CLI edge).
package main

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/spacemanager"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
	"github.com/lalternativefabrique/sklp/spacemanager/gateway/redirect"
	"github.com/lalternativefabrique/sklp/spacemanager/mux"

	// Blank import: register the traefik gateway driver, same single wiring
	// point as cmd/sklet. Without it every `sklp gateway up|down|status` fails
	// with "driver not linked" — there is no other binary an operator can drive
	// the host gateway singleton with.
	_ "github.com/lalternativefabrique/sklp/spacemanager/gateway/traefik"
)

// gatewayCommand declares `sklp gateway <up|down|status>` on the façade. Each
// sub-verb takes -file/-repo, selecting the deploy.yaml the driver is read from.
func gatewayCommand() cli.Command {
	gwFlags := []cli.Flag{
		{Name: "file", Type: cli.StringFlag, StringDefault: filepath.Join(".sklp", "deploy.yaml"), Usage: "path to deploy.yaml (selects the gateway driver)"},
		{Name: "repo", Type: cli.StringFlag, StringDefault: ".", Usage: "repository root the deploy file is relative to"},
	}
	sub := func(name, short string) cli.Command {
		return cli.Command{
			Name:  name,
			Short: short,
			Flags: append(append([]cli.Flag{}, gwFlags...), netFlagGroup()...),
			Run: func(ctx context.Context, args cli.Args) int {
				// An EXPLICIT remote target (-context or -server) pilots the
				// NODE's gateway over its control API — no SSH, no local
				// driver. Unlike deploy, the active context alone does not
				// flip this verb remote: `gateway up` run on the node itself
				// must keep addressing the local host.
				if args.String("context") != "" || args.String("server") != "" {
					return runGatewayRemote(ctx, name, netFlagsFromArgs(args))
				}
				return runGateway(name, args.String("file"), args.String("repo"))
			},
		}
	}
	muxCmd := cli.Command{
		Name:  "mux",
		Short: "Run the host front-door mux (ingress-per-space): route external traffic to each space's ingress",
		Flags: []cli.Flag{
			{Name: "listen", Type: cli.StringFlag, StringDefault: ":9080", Usage: "HTTP front door address (routes by Host header)"},
			{Name: "listen-tls", Type: cli.StringFlag, StringDefault: ":9443", Usage: "HTTPS front door address (routes by TLS SNI, passthrough); empty to disable"},
		},
		Run: func(ctx context.Context, args cli.Args) int {
			return runGatewayMux(ctx, args.String("listen"), args.String("listen-tls"))
		},
	}
	persistFlag := []cli.Flag{
		{Name: "persist", Type: cli.BoolFlag, BoolDefault: false, Usage: "also write the rules under /etc/nftables.d so they survive a reboot"},
	}
	redirectCmd := cli.Command{
		Name:  "redirect",
		Short: "Manage the host :80->:9080 / :443->:9443 NAT redirect (front door for the rootless gateway)",
		Sub: []cli.Command{
			{Name: "up", Short: "Install the front-door redirect (needs root: run via sudo)", Flags: persistFlag, Run: func(ctx context.Context, args cli.Args) int {
				return runGatewayRedirect("up", args.Bool("persist"))
			}},
			{Name: "down", Short: "Remove the front-door redirect", Flags: persistFlag, Run: func(ctx context.Context, args cli.Args) int {
				return runGatewayRedirect("down", args.Bool("persist"))
			}},
			{Name: "status", Short: "Show whether the front-door redirect is installed", Run: func(ctx context.Context, args cli.Args) int {
				return runGatewayRedirect("status", false)
			}},
		},
	}
	return cli.Command{
		Name:  "gateway",
		Short: "Manage the host ingress gateway (up | down | status | mux | redirect)",
		Sub: []cli.Command{
			sub("up", "Bring the host gateway singleton up"),
			sub("down", "Tear the host gateway singleton down"),
			sub("status", "Show the host gateway singleton state"),
			muxCmd,
			redirectCmd,
		},
	}
}

// runGatewayRedirect installs/removes/queries the host front-door NAT that maps
// the public :80/:443 onto the unprivileged :9080/:9443 the rootless gateway
// binds. nft needs CAP_NET_ADMIN, so up/down must run as root (sudo); a
// permission error from nft is surfaced verbatim so the operator knows to elevate.
func runGatewayRedirect(sub string, persist bool) int {
	switch sub {
	case "up":
		if err := redirect.Install(); err != nil {
			fmt.Fprintln(os.Stderr, "sklp gateway redirect up:", err)
			return 1
		}
		fmt.Fprintln(os.Stdout, "sklp gateway: front-door redirect installed (:80->:9080, :443->:9443)")
		if persist {
			if err := redirect.Persist(); err != nil {
				fmt.Fprintln(os.Stderr, "sklp gateway redirect up: persist:", err)
				return 1
			}
			fmt.Fprintf(os.Stdout, "sklp gateway: rules persisted to %s\n", redirect.PersistPath())
			if !redirect.MainConfigSourcesDropIn() {
				fmt.Fprintln(os.Stdout, "  note: /etc/nftables.conf does not include /etc/nftables.d — add")
				fmt.Fprintln(os.Stdout, "        `include \"/etc/nftables.d/*.conf\"` and enable the nftables service")
				fmt.Fprintln(os.Stdout, "        (`systemctl enable --now nftables`) for the rules to load on boot.")
			}
		}
	case "down":
		if err := redirect.Remove(); err != nil {
			fmt.Fprintln(os.Stderr, "sklp gateway redirect down:", err)
			return 1
		}
		fmt.Fprintln(os.Stdout, "sklp gateway: front-door redirect removed")
		if persist {
			if err := redirect.Unpersist(); err != nil {
				fmt.Fprintln(os.Stderr, "sklp gateway redirect down: unpersist:", err)
				return 1
			}
			fmt.Fprintf(os.Stdout, "sklp gateway: persisted rules removed (%s)\n", redirect.PersistPath())
		}
	case "status":
		fmt.Fprintf(os.Stdout, "redirect: %s\n", redirectStatusLine())
	}
	return 0
}

// redirectStatusLine renders the front-door redirect state for `redirect status`
// and `gateway status`, distinguishing a definite "not installed" from an
// "unknown (run as root to query)" so the line never falsely reads absent when
// it merely lacked permission to look.
func redirectStatusLine() string {
	switch st, _ := redirect.Status(); st {
	case redirect.StateInstalled:
		return "installed (:80->:9080, :443->:9443)"
	case redirect.StateAbsent:
		return "not installed"
	default:
		return "unknown (run as root to query)"
	}
}

// runGatewayMux runs the ingress-per-space front door: it binds listen, builds
// the host->space table from every live space's manifest, and serves until the
// context is cancelled (Ctrl-C). It rescans the table periodically so spaces
// that deploy/teardown after start are picked up without a restart. Foreground
// by design for now (a detached supervisor can wrap it like deploy does).
func runGatewayMux(ctx context.Context, listen, listenTLS string) int {
	// The loop lives in the mux package so sklet runs the identical front door:
	// the unit that starts it is written with whichever binary is on the node.
	if err := mux.Run(ctx, listen, listenTLS, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "sklp gateway mux:", err)
		return 1
	}
	return 0
}

// runGatewayRemote pilots a NODE's gateway singleton over its control API:
// status reads it, up (re-)ensures it (a fresh traefik boot also retries any
// missing ACME cert). down is deliberately not remote — tearing the front door
// out from under every space on a node is a decision to make ON the node.
func runGatewayRemote(ctx context.Context, sub string, nf *netFlags) int {
	if sub == "down" {
		fmt.Fprintln(os.Stderr, "sklp gateway down: not available remotely — run it on the node itself")
		return 1
	}
	client, err := nf.dialClient("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp gateway:", err)
		return 1
	}
	var st control.GatewayControlStatus
	switch sub {
	case "up":
		st, err = client.GatewayControlUp(ctx)
	default:
		st, err = client.GatewayControlStatus(ctx)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp gateway "+sub+":", err)
		return 1
	}
	state := "stopped"
	if st.Running {
		state = "running"
	}
	fmt.Printf("driver:    %s\nstate:     %s\nlistening: %s\n", st.Driver, state, st.Listening)
	return 0
}

// runGateway opens the configured gateway driver and runs the chosen sub-verb,
// returning the process exit code.
func runGateway(sub, file, repo string) int {
	// The gateway singleton is a ROOTLESS container owned by the invoking user.
	// If this runs under sudo (a natural reflex, since `redirect` needs root),
	// the runc state root resolves to root's empty HOME and the driver query
	// would falsely report "not running". Re-root HOME/XDG to the original user
	// so up/down/status address the owning user's runtime, not root's.
	rerootToSudoUser()

	driver, err := gatewayDriverFromConfig(file, repo)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp gateway:", err)
		return 1
	}

	gw, err := spacemanager.OpenGateway(driver)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp gateway:", err)
		return 1
	}

	switch sub {
	case "up":
		if err := gw.EnsureUp(); err != nil {
			fmt.Fprintln(os.Stderr, "sklp gateway up:", err)
			return 1
		}
		fmt.Fprintf(os.Stdout, "sklp gateway: %s driver is up\n", driver)
	case "down":
		if err := gw.Down(); err != nil {
			fmt.Fprintln(os.Stderr, "sklp gateway down:", err)
			return 1
		}
		// Also tear down the per-space-mode front door (the mux unit), so a single
		// `gateway down` stops BOTH front doors regardless of mode. Best-effort.
		if err := control.StopMuxUnit(); err != nil {
			fmt.Fprintln(os.Stderr, "sklp gateway down: mux:", err)
		}
		fmt.Fprintf(os.Stdout, "sklp gateway: %s driver is down\n", driver)
	case "status":
		st, err := gw.Status()
		if err != nil {
			fmt.Fprintln(os.Stderr, "sklp gateway status:", err)
			return 1
		}
		state := "not running"
		if st.Running {
			state = "running"
		}
		fmt.Fprintf(os.Stdout, "driver:    %s\n", st.Driver)
		fmt.Fprintf(os.Stdout, "state:     %s\n", state)
		fmt.Fprintf(os.Stdout, "listening: %s\n", st.Listening)
		// Report the per-space-mode mux front door too (the host singleton view
		// above only covers the legacy driver).
		fmt.Fprintf(os.Stdout, "mux:       %s\n", muxState())
		// And the host front-door NAT redirect (:80->:9080 / :443->:9443).
		fmt.Fprintf(os.Stdout, "redirect:  %s\n", redirectStatusLine())
	}
	return 0
}

// rerootToSudoUser, when the process runs under sudo, points HOME (and clears
// the XDG cache/runtime overrides sudo may have inherited from root) at the
// original $SUDO_USER's home, so the rootless runtime's state root resolves to
// the user that actually owns the gateway containers. A no-op when not under
// sudo or when the user can't be resolved (best-effort: a wrong reroot is worse
// than none, so any lookup failure leaves the environment untouched).
func rerootToSudoUser() {
	name := os.Getenv("SUDO_USER")
	if name == "" || name == "root" {
		return
	}
	u, err := user.Lookup(name)
	if err != nil || u.HomeDir == "" {
		return
	}
	os.Setenv("HOME", u.HomeDir)
	// XDG_CACHE_HOME drives the runtime state root (see space.cacheRoot); if sudo
	// carried root's value, drop it so it re-derives from the corrected HOME.
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" && !strings.HasPrefix(v, u.HomeDir) {
		os.Unsetenv("XDG_CACHE_HOME")
	}
}

// muxState reports the host mux unit state for `gateway status`, in one line.
func muxState() string {
	if control.MuxUnitActive() {
		return "running (" + control.MuxUnitName() + ")"
	}
	return "not running"
}

// gatewayDriverFromConfig reads the gateway driver from deploy.yaml. A missing
// deploy.yaml is NOT fatal — the gateway is a host singleton independent of any
// one space, so it falls back to the default driver so `gateway status` works on
// a fresh host. A present-but-invalid config still errors (validation catches an
// unknown driver).
func gatewayDriverFromConfig(file, repo string) (string, error) {
	path := file
	if !filepath.IsAbs(path) {
		path = filepath.Join(repo, path)
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return spacemanager.DefaultGatewayDriver, nil
		}
		return "", err
	}
	cfg, err := spacemanager.Load(path)
	if err != nil {
		return "", err
	}
	return cfg.GatewayDriverName(), nil
}
