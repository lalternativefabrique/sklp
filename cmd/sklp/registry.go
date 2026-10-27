// registry.go implements `sklp registry {login,ls,logout}` — the ONLY way a
// push credential reaches sklp. The on-disk file is owned by
// internal/registry; this file is the verb surface.
//
// sklp never reads ~/.docker/config.json: a credential this verb did not
// write does not exist, so a stale docker login cannot authenticate a push
// behind sklp's back.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/internal/registry"
)

// registryProvider pre-fills host and username for a well-known registry, so
// the common login is one flag instead of two exact strings. It carries no
// credential: the password always comes from the prompt or stdin.
type registryProvider struct {
	Host     string
	Username string
}

var registryProviders = map[string]registryProvider{
	"scaleway":   {Host: "rg.fr-par.scw.cloud", Username: "nologin"},
	"github":     {Host: "ghcr.io", Username: "oauth2"},
	"docker-hub": {Host: "docker.io"},
}

func registryProviderNames() []string {
	out := make([]string, 0, len(registryProviders))
	for n := range registryProviders {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func registryCommand() cli.Command {
	return cli.Command{
		Name:  "registry",
		Short: "Manage container registry credentials: login | ls | logout",
		Long: "Registry credentials live in $XDG_CONFIG_HOME/sklp/registries.yaml (0600).\n" +
			"They are the only source a push reads — sklp does not consult ~/.docker/config.json.",
		Sub: []cli.Command{
			{
				Name:  "login",
				Short: "Save a registry credential (host from --provider or the positional)",
				Flags: []cli.Flag{
					{Name: "provider", Aliases: []string{"p"}, Type: cli.StringFlag,
						Usage: "pre-fill host and username: " + strings.Join(registryProviderNames(), " | ")},
					{Name: "username", Aliases: []string{"u"}, Type: cli.StringFlag, Usage: "registry username"},
					{Name: "password-stdin", Type: cli.BoolFlag, Usage: "read the password from stdin instead of prompting"},
					{Name: "credential-command", Type: cli.StringFlag,
						Usage: "store a lookup command whose stdout is the password, instead of the password itself"},
				},
				Positionals: []cli.Positional{{Name: "host"}},
				Run: func(ctx context.Context, args cli.Args) int {
					err := registryLogin(defaultAuthIO(), registryLoginOpts{
						Host:              positionalOrEmpty(args),
						Provider:          args.String("provider"),
						Username:          args.String("username"),
						PasswordStdin:     args.Bool("password-stdin"),
						CredentialCommand: args.String("credential-command"),
					})
					if err != nil {
						fmt.Fprintln(os.Stderr, "sklp registry:", err)
						return 1
					}
					return 0
				},
			},
			{
				Name:  "ls",
				Short: "List configured registries (never prints a secret)",
				Run: func(ctx context.Context, args cli.Args) int {
					if err := registryList(os.Stdout); err != nil {
						fmt.Fprintln(os.Stderr, "sklp registry:", err)
						return 1
					}
					return 0
				},
			},
			{
				Name:        "logout",
				Short:       "Remove a registry credential",
				Positionals: []cli.Positional{{Name: "host", Required: true}},
				Run: func(ctx context.Context, args cli.Args) int {
					if err := registryLogout(os.Stdout, positionalOrEmpty(args)); err != nil {
						fmt.Fprintln(os.Stderr, "sklp registry:", err)
						return 1
					}
					return 0
				},
			},
		},
	}
}

func positionalOrEmpty(args cli.Args) string {
	v, _ := firstPositional(args)
	return v
}

type registryLoginOpts struct {
	Host              string
	Provider          string
	Username          string
	PasswordStdin     bool
	CredentialCommand string
}

// registryLogin resolves host and username (positional and flags win over the
// provider's defaults), reads the password unless a credential_command was
// given instead, and upserts the entry.
func registryLogin(aio *authIO, opts registryLoginOpts) error {
	host := strings.TrimSpace(opts.Host)
	username := strings.TrimSpace(opts.Username)

	if p := strings.TrimSpace(opts.Provider); p != "" {
		prov, ok := registryProviders[p]
		if !ok {
			return fmt.Errorf("unknown provider %q (known: %s)", p, strings.Join(registryProviderNames(), ", "))
		}
		if host == "" {
			host = prov.Host
		}
		if username == "" {
			username = prov.Username
		}
	}
	if host == "" {
		return errors.New("a registry host is required (positional, or --provider to pre-fill it)")
	}
	if username == "" {
		return errors.New("--username is required (Scaleway mandates \"nologin\")")
	}

	cred := registry.Credential{Username: username}
	if cmd := strings.TrimSpace(opts.CredentialCommand); cmd != "" {
		if opts.PasswordStdin {
			return errors.New("--credential-command and --password-stdin are mutually exclusive")
		}
		// Fail now rather than at the next push: a command that cannot produce
		// a password is a typo, and the login is where the user can fix it.
		probe := registry.File{Registries: map[string]registry.Credential{
			host: {Username: username, CredentialCommand: cmd},
		}}
		if _, err := probe.Resolve(host); err != nil {
			return err
		}
		cred.CredentialCommand = cmd
	} else {
		password, err := readRegistryPassword(aio, host, opts.PasswordStdin)
		if err != nil {
			return err
		}
		if password == "" {
			return errors.New("aborted: empty password")
		}
		cred.Auth = registry.EncodeAuth(username, password)
	}

	f, path, err := registry.Load()
	if err != nil {
		return err
	}
	f.Set(host, cred)
	if err := f.Save(path); err != nil {
		return fmt.Errorf("save %s: %w", path, err)
	}
	fmt.Fprintf(aio.Stdout, "saved credential for %s (user %s) in %s\n", host, username, path)
	return nil
}

func readRegistryPassword(aio *authIO, host string, fromStdin bool) (string, error) {
	if fromStdin {
		line, err := readLine(aio.Stdin)
		if err != nil {
			return "", fmt.Errorf("read password from stdin: %w", err)
		}
		return strings.TrimSpace(line), nil
	}
	fmt.Fprintf(aio.Stdout, "Password for %s: ", host)
	password, err := readPassword(aio)
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	fmt.Fprintln(aio.Stdout)
	return strings.TrimSpace(password), nil
}

// registryList prints host, username and where the secret comes from. The
// secret itself is never rendered, in any form.
func registryList(out *os.File) error {
	f, path, err := registry.Load()
	if err != nil {
		return err
	}
	hosts := f.Hosts()
	if len(hosts) == 0 {
		fmt.Fprintf(out, "no registry credentials in %s\n", path)
		fmt.Fprintln(out, "  run: sklp registry login <host> --provider <name>")
		return nil
	}
	fmt.Fprintf(out, "%-34s %-14s %s\n", "HOST", "USERNAME", "SOURCE")
	for _, h := range hosts {
		c := f.Registries[h]
		source := "stored"
		if c.CredentialCommand != "" {
			source = "command"
		}
		fmt.Fprintf(out, "%-34s %-14s %s\n", h, c.Username, source)
	}
	return nil
}

func registryLogout(out *os.File, host string) error {
	host = strings.TrimSpace(host)
	f, path, err := registry.Load()
	if err != nil {
		return err
	}
	if !f.Delete(host) {
		return fmt.Errorf("no credential for %q", host)
	}
	if err := f.Save(path); err != nil {
		return fmt.Errorf("save %s: %w", path, err)
	}
	fmt.Fprintf(out, "removed credential for %s\n", host)
	return nil
}
