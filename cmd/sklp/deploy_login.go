// deploy_login.go holds the client-side `sklp deploy login`: it persists the
// remote control transport (server URL + Bearer token + pinned cert fingerprint)
// into a deploy CONTEXT so subsequent `sklp deploy ls/logs/exec/...` need no
// flags. It is the copy-paste target the supervisor prints when started with
// `--listen`. It never touches the "api" block — SaaS auth is `sklp auth login`.
// Non-gated: it only writes config, no embedded runtime needed.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/internal/config"
)

// deployLoginCommand declares `sklp deploy login`: save server + token +
// ca-fingerprint into the named (or active) deploy context.
func deployLoginCommand() cli.Command {
	return cli.Command{
		Name:  "login",
		Short: "Save remote control credentials (server, token, ca-fingerprint) into a deploy context",
		Flags: []cli.Flag{
			{Name: "server", Aliases: []string{"addr"}, Type: cli.StringFlag, Usage: "remote supervisor control API (https://host:9443)"},
			{Name: "token", Type: cli.StringFlag, Usage: "Bearer token for the control API"},
			{Name: "ca-fingerprint", Type: cli.StringFlag, Usage: "pinned server certificate sha256 fingerprint"},
			{Name: "space", Type: cli.StringFlag, Usage: "default space for this context (like a kubeconfig namespace); a plain `sklp deploy` targets it"},
			{Name: "context", Type: cli.StringFlag, Usage: "context to write (default: the active context, or \"default\" when none is set)"},
		},
		Run: func(ctx context.Context, args cli.Args) int {
			err := deployLogin(args.String("context"), args.String("server"), args.String("token"), args.String("ca-fingerprint"), args.String("space"))
			if err != nil {
				fmt.Fprintln(os.Stderr, "sklp deploy login:", err)
				return 1
			}
			return 0
		},
	}
}

// deployLogin upserts the remote control transport into a deploy context, then
// saves 0600 atomically. server + token are required; ca-fingerprint is
// recommended but optional (LAN bootstrap may rely on --insecure at call time).
func deployLogin(contextName, server, token, caFingerprint, space string) error {
	server = strings.TrimSpace(server)
	token = strings.TrimSpace(token)
	caFingerprint = strings.TrimSpace(caFingerprint)
	space = strings.TrimSpace(space)
	if server == "" {
		return fmt.Errorf("--server is required (e.g. https://host:9443)")
	}
	if token == "" {
		return fmt.Errorf("--token is required")
	}

	cfg, path, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// We do NOT go through ResolveContext here: its "nothing selected" error is
	// the right answer for verbs that must hit an existing node, but login is
	// how the FIRST context comes into existence. Falling back to "default"
	// gives a name to write; SetContext then makes it current if none was.
	name := contextName
	if name == "" {
		name = os.Getenv(config.EnvContext)
	}
	if name == "" {
		name = cfg.CurrentContext
	}
	if name == "" {
		name = "default"
	}

	// Read-modify-write so re-running login with only --token keeps the space
	// (and any other field) already recorded for this context.
	c := cfg.Contexts[name]
	c.Server = server
	c.Token = token
	c.CAFingerprint = caFingerprint
	if space != "" {
		c.Space = space
	}
	cfg.SetContext(name, c)

	if err := cfg.Save(path); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	fmt.Printf("saved control credentials to context %q (%s)\n", name, server)
	return nil
}
