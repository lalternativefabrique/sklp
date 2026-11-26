// deploy_netflags.go is the NON-gated client-side plumbing for the network
// control transport (`--server`/`--token`/`--ca-fingerprint`/`--insecure`). It is
// shared by the gated deploy sub-verbs (ps/logs/exec/down/restart/apply) and by
// the non-gated interceptor that lets a binary built WITHOUT the embedded runtime
// pilot a remote supervisor (mirrors deploy_remote.go's ssh client). Only the
// SERVER (netserve/tls/token) needs the embedded runtime.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/internal/config"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
)

// netFlagGroup is the reusable façade declaration of the client-side network
// transport flags (--server/--addr alias, --token, --token-file, --ca-fingerprint,
// --insecure). Append it into every deploy sub-verb that pilots a supervisor.
func netFlagGroup() []cli.Flag {
	return []cli.Flag{
		{Name: "server", Aliases: []string{"addr"}, Type: cli.StringFlag, Usage: "remote supervisor control API (https://host:9443); when set, pilot it over the network instead of the local socket"},
		{Name: "token", Type: cli.StringFlag, Usage: "Bearer token for --server (prefer --token-file or SKLP_CONTEXT_TOKEN to keep it out of the process list)"},
		{Name: "token-file", Type: cli.StringFlag, Usage: "file holding the Bearer token for --server"},
		{Name: "ca-fingerprint", Type: cli.StringFlag, Usage: "pin the server certificate by its sha256 fingerprint (printed by the server / `sklp deploy fingerprint`)"},
		{Name: "insecure", Type: cli.BoolFlag, Usage: "skip TLS verification for --server (LAN bootstrap only; prefer --ca-fingerprint)"},
		{Name: "context", Type: cli.StringFlag, Usage: "deploy context to read server/token/ca-fingerprint from (default: the active context)"},
		{Name: "config", Type: cli.StringFlag, Usage: "path to a config file to read (one file per node/VM, e.g. shop.yaml); overrides the default ~/.config/sklp/config.yaml"},
	}
}

// netFlagsFromArgs reconstructs the network transport config from parsed façade
// args, by the same rules bindNetFlags wired on a flag.FlagSet.
func netFlagsFromArgs(args cli.Args) *netFlags {
	// --config points the config loader at a specific file (one per node/VM).
	// Set it BEFORE applyContextDefaults, which loads the config via Path().
	applyConfigFlag(args.String("config"))
	nf := &netFlags{
		server:    args.String("server"),
		token:     args.String("token"),
		tokenFile: args.String("token-file"),
		caFinger:  args.String("ca-fingerprint"),
		insecure:  args.Bool("insecure"),
		context:   args.String("context"),
	}
	nf.applyContextDefaults()
	return nf
}

// applyConfigFlag makes --config take effect by exporting SKLP_CONFIG, which
// config.Path() honours first. A no-op when empty, so the default location is
// used. Env is the wiring so every config.Load() in the process — including the
// plain-deploy router's — sees the same file without threading a path through.
func applyConfigFlag(path string) {
	if path != "" {
		_ = os.Setenv("SKLP_CONFIG", path)
	}
}

// applyContextDefaults fills server / ca-fingerprint / token from the active (or
// --context) deploy context when the corresponding flag is absent — explicit
// flags and env always win. Best-effort: a missing/unreadable config, or no
// context selected, is ignored so a flag-only invocation never depends on disk
// state. Idempotent.
//
// Leaving server empty when nothing resolves is safe here: netFlags.remote()
// then reports false and the caller takes the LOCAL socket path only because
// no --server was ever requested — this function never downgrades a resolved
// remote target.
func (nf *netFlags) applyContextDefaults() {
	if nf.server != "" && nf.caFinger != "" && nf.token != "" {
		return
	}
	cfg, _, err := config.Load()
	if err != nil {
		return
	}
	_, c, err := cfg.ResolveContext(nf.context)
	if err != nil {
		return
	}
	if nf.server == "" {
		nf.server = c.Server
	}
	if nf.caFinger == "" {
		nf.caFinger = c.CAFingerprint
	}
	if nf.token == "" && nf.tokenFile == "" {
		// EffectiveContextToken, NOT EffectiveAPIToken: this bearer drives a
		// node's deploy control API, and must not be satisfiable by a SaaS PAT
		// exported as SKLP_TOKEN for `sklp issue`.
		nf.token = config.EffectiveContextToken(c)
	}
}

// parseWithPositional parses fs from args while tolerating ONE positional
// argument placed anywhere (before, between, or after flags). Go's flag.Parse
// stops at the first non-flag, so `logs db -f` would drop `-f`; here we parse,
// lift the leading positional flag.Parse stopped on, then re-parse the tail so
// `sklp deploy logs db -f` and `... -f db` both behave like `docker logs`.
// Returns the positional ("" if none) — extra positionals are an error.
func parseWithPositional(fs *flag.FlagSet, args []string) (string, error) {
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return "", nil
	}
	positional := rest[0]
	if err := fs.Parse(rest[1:]); err != nil {
		return "", err
	}
	if extra := fs.Args(); len(extra) > 0 {
		return "", fmt.Errorf("unexpected extra argument %q", extra[0])
	}
	return positional, nil
}

// splitDoubleDash splits args at the first bare "--": before is returned as the
// flag/positional args, after as the command argv. With no "--", after is nil.
func splitDoubleDash(args []string) (before, after []string) {
	for i, a := range args {
		if a == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}

// netFlags holds the client-side network transport flags. When server is set the
// CLI talks to a remote supervisor over HTTPS+token instead of the local socket.
type netFlags struct {
	server     string // https://host:port (alias --addr); "" = local socket
	token      string // Bearer token value (prefer --token-file / SKLP_CONTEXT_TOKEN)
	tokenFile  string // path to a file holding the token
	caFinger   string // pinned server cert sha256 (TOFU)
	insecure   bool   // skip TLS verification (LAN bootstrap)
	context    string // deploy context to read defaults from ("" = active)
	configFile string // --config: path to a specific config file ("" = default)
}

// bindNetFlags registers the network transport flags on fs. Call before Parse.
func bindNetFlags(fs *flag.FlagSet) *netFlags {
	nf := &netFlags{}
	fs.StringVar(&nf.server, "server", "", "remote supervisor control API (https://host:9443); when set, pilot it over the network instead of the local socket")
	fs.StringVar(&nf.server, "addr", "", "alias of --server")
	fs.StringVar(&nf.token, "token", "", "Bearer token for --server (prefer --token-file or SKLP_CONTEXT_TOKEN to keep it out of the process list)")
	fs.StringVar(&nf.tokenFile, "token-file", "", "file holding the Bearer token for --server")
	fs.StringVar(&nf.caFinger, "ca-fingerprint", "", "pin the server certificate by its sha256 fingerprint (printed by the server / `sklp deploy fingerprint`)")
	fs.BoolVar(&nf.insecure, "insecure", false, "skip TLS verification for --server (LAN bootstrap only; prefer --ca-fingerprint)")
	fs.StringVar(&nf.context, "context", "", "deploy context to read server/token/ca-fingerprint from (default: the active context)")
	fs.StringVar(&nf.configFile, "config", "", "path to a config file to read (one file per node/VM); overrides ~/.config/sklp/config.yaml")
	return nf
}

// remote reports whether a network transport was requested (--server/--addr set).
func (nf *netFlags) remote() bool { return nf.server != "" }

// resolveToken returns the token from (in order) --token-file, --token, or
// $SKLP_CONTEXT_TOKEN. Returns an error when none is set (the network transport
// requires one — there is no anonymous access).
//
// The env var is the CONTEXT one, not SKLP_TOKEN: the latter is the SaaS PAT,
// and letting it authenticate a node's control API meant a CI token exported
// for `sklp issue` silently doubled as deploy credentials.
func (nf *netFlags) resolveToken() (string, error) {
	if nf.tokenFile != "" {
		data, err := os.ReadFile(nf.tokenFile)
		if err != nil {
			return "", fmt.Errorf("read --token-file: %w", err)
		}
		if tok := strings.TrimSpace(string(data)); tok != "" {
			return tok, nil
		}
		return "", fmt.Errorf("--token-file %q is empty", nf.tokenFile)
	}
	if nf.token != "" {
		return nf.token, nil
	}
	if env := strings.TrimSpace(os.Getenv(config.EnvContextToken)); env != "" {
		return env, nil
	}
	return "", fmt.Errorf("a token is required for --server (use --token-file, %s, or --token)", config.EnvContextToken)
}

// dialClient returns a control.Client for the chosen transport: DialNet (remote,
// over --server with a token + TLS trust) when --server is set, else the local
// Dial over the space socket. space labels the client for error messages.
func (nf *netFlags) dialClient(space string) (*control.Client, error) {
	applyConfigFlag(nf.configFile)
	nf.applyContextDefaults()
	if !nf.remote() {
		return control.Dial(space)
	}
	tok, err := nf.resolveToken()
	if err != nil {
		return nil, err
	}
	var opts []control.DialOption
	if nf.caFinger != "" {
		opts = append(opts, control.WithCAFingerprint(nf.caFinger))
	}
	if nf.insecure {
		opts = append(opts, control.WithInsecure())
	}
	if nf.caFinger == "" && !nf.insecure {
		return nil, fmt.Errorf("--server requires either --ca-fingerprint <sha256> (recommended) or --insecure (LAN bootstrap)")
	}
	c, err := control.DialNet(nf.server, tok, opts...)
	if err != nil {
		return nil, err
	}
	return c.WithSpace(space), nil
}
