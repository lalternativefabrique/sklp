// deploy_netadmin_embedded.go holds the server-side network admin sub-verbs:
//
//	sklp deploy token --space X        print the space's Bearer token
//	sklp deploy fingerprint --space X  print the space's TLS cert fingerprint
//
// Both ensure the material exists (generating it on first use, idempotent) and
// print the value to copy into a remote client / CI / the UI. They are gated on
// embedded_runtime because they manage the SERVER's cert/token (the same files
// the --listen supervisor uses); the network CLIENT (DialNet) stays non-gated.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/internal/config"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
)

// deployTokenCommand declares `sklp deploy token`: print the space's network
// Bearer token, generating+persisting one on first use.
func deployTokenCommand() cli.Command {
	return cli.Command{
		Name:  "token",
		Short: "Print the space's network Bearer token",
		Flags: []cli.Flag{{Name: "space", Type: cli.StringFlag, StringDefault: "production", Usage: "space whose token to print"}},
		Run: func(ctx context.Context, args cli.Args) int {
			tok, err := control.EnsureToken(args.String("space"))
			if err != nil {
				fmt.Fprintln(os.Stderr, "sklp deploy token:", err)
				return 1
			}
			fmt.Println(tok)
			return 0
		},
		Sub: []cli.Command{deployTokenRotateCommand()},
	}
}

// deployTokenRotateCommand declares `sklp deploy token rotate`: rotate a
// remote node's token over its control API, authenticated with the current
// one, and save the new one into the context before printing anything.
func deployTokenRotateCommand() cli.Command {
	return cli.Command{
		Name:  "rotate",
		Short: "Rotate a node's control API token through its API and save the new one into the deploy context",
		Flags: []cli.Flag{
			{Name: "context", Type: cli.StringFlag, Usage: "deploy context to rotate (default: the active one)"},
		},
		Run: func(ctx context.Context, args cli.Args) int {
			if err := deployTokenRotate(ctx, args.String("context")); err != nil {
				fmt.Fprintln(os.Stderr, "sklp deploy token rotate:", err)
				return 1
			}
			return 0
		},
	}
}

func deployTokenRotate(ctx context.Context, contextName string) error {
	cfg, path, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	name, c, err := cfg.ResolveContext(contextName)
	if err != nil {
		return err
	}
	if c.Server == "" {
		return fmt.Errorf("context %q has no server", name)
	}
	var opts []control.DialOption
	if c.CAFingerprint != "" {
		opts = append(opts, control.WithCAFingerprint(c.CAFingerprint))
	}
	client, err := control.DialNet(c.Server, config.EffectiveContextToken(c), opts...)
	if err != nil {
		return err
	}
	tok, err := client.RotateToken(ctx)
	if err != nil {
		return err
	}
	c.Token = tok
	cfg.SetContext(name, c)
	if err := cfg.Save(path); err != nil {
		return fmt.Errorf("token rotated on %s but NOT saved (%v); record it now:\n  sklp deploy login --context %s --server %s --token %s", c.Server, err, name, c.Server, tok)
	}
	fmt.Printf("rotated the token of context %q (%s); the previous token no longer works.\n", name, c.Server)
	fmt.Printf("Anything else holding the old token must be updated: Skalpai core's node registration, other workstations, CI.\n")
	return nil
}

// deployFingerprintCommand declares `sklp deploy fingerprint`: print the SHA-256
// fingerprint of the space's TLS server certificate (the value a client pins via
// --ca-fingerprint), generating the cert on first use.
func deployFingerprintCommand() cli.Command {
	return cli.Command{
		Name:  "fingerprint",
		Short: "Print the space's TLS cert fingerprint",
		Flags: []cli.Flag{{Name: "space", Type: cli.StringFlag, StringDefault: "production", Usage: "space whose cert fingerprint to print"}},
		Run: func(ctx context.Context, args cli.Args) int {
			_, fp, err := control.EnsureServerCert(args.String("space"))
			if err != nil {
				fmt.Fprintln(os.Stderr, "sklp deploy fingerprint:", err)
				return 1
			}
			fmt.Println(fp)
			return 0
		},
	}
}
