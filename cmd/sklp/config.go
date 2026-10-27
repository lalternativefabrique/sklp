package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/internal/stackconfig"
)

// config.go implements `sklp config` — the Pulumi-style per-stack config store:
// one file .sklp/<stack>/config.yaml holding plain values and secrets encrypted
// (secure:) under SKLP_SECRETS_PASSPHRASE. The deploy path resolves ${VAR} from
// it. Commands: set / get / ls / rm / import.

func configCommand() cli.Command {
	stackFlag := cli.Flag{Name: "stack", Aliases: []string{"s"}, Type: cli.StringFlag, StringDefault: "prod", Usage: "stack whose .sklp/<stack>/config.yaml to operate on"}
	return cli.Command{
		Name:  "config",
		Short: "Manage a stack's config + secrets (.sklp/<stack>/config.yaml, Pulumi-style)",
		Long: "One file per stack holds plain config and encrypted secrets (secure:).\n" +
			"Secrets are AES-256-GCM, keyed by SKLP_SECRETS_PASSPHRASE — the file is\n" +
			"committable, useless without the passphrase. Deploy resolves ${VAR} from it.",
		Run: func(ctx context.Context, args cli.Args) int { return runConfigList("prod") },
		Sub: []cli.Command{
			{
				Name:        "set",
				Short:       "Set a value (plain, or --secret to encrypt it)",
				Flags:       []cli.Flag{stackFlag, {Name: "secret", Type: cli.BoolFlag, Usage: "encrypt the value (stored as secure:)"}},
				Positionals: []cli.Positional{{Name: "key"}, {Name: "value"}},
				Run: func(ctx context.Context, args cli.Args) int {
					return runConfigSet(args.String("stack"), firstArg(args, 0), firstArg(args, 1), args.Bool("secret"))
				},
			},
			{
				Name:        "get",
				Short:       "Print a value (secrets require the passphrase; use --show)",
				Flags:       []cli.Flag{stackFlag, {Name: "show", Type: cli.BoolFlag, Usage: "decrypt and print a secret value in clear"}},
				Positionals: []cli.Positional{{Name: "key"}},
				Run: func(ctx context.Context, args cli.Args) int {
					return runConfigGet(args.String("stack"), firstArg(args, 0), args.Bool("show"))
				},
			},
			{
				Name:  "ls",
				Short: "List configured keys (secrets shown as <secret>)",
				Flags: []cli.Flag{stackFlag},
				Run:   func(ctx context.Context, args cli.Args) int { return runConfigList(args.String("stack")) },
			},
			{
				Name:        "rm",
				Short:       "Remove a key",
				Flags:       []cli.Flag{stackFlag},
				Positionals: []cli.Positional{{Name: "key"}},
				Run: func(ctx context.Context, args cli.Args) int {
					return runConfigRm(args.String("stack"), firstArg(args, 0))
				},
			},
			{
				Name:        "import",
				Short:       "Bulk-import a dotenv file (KEY=VALUE per line); --secret encrypts all",
				Flags:       []cli.Flag{stackFlag, {Name: "secret", Type: cli.BoolFlag, Usage: "encrypt every imported value (for a secrets file)"}},
				Positionals: []cli.Positional{{Name: "file"}},
				Run: func(ctx context.Context, args cli.Args) int {
					return runConfigImport(args.String("stack"), firstArg(args, 0), args.Bool("secret"))
				},
			},
			{
				Name:  "rotate",
				Short: "Re-seal every secret under a new passphrase (new salt, current KDF cost)",
				Long: "Reads the file with " + stackconfig.PassphraseEnv + " and rewrites it under\n" +
					stackconfig.NewPassphraseEnv + ", drawing a fresh salt. No value is ever printed\n" +
					"or passed on the command line. Also upgrades a file written at the old KDF\n" +
					"cost to the current one.",
				Flags: []cli.Flag{stackFlag},
				Run: func(ctx context.Context, args cli.Args) int {
					return runConfigRotate(args.String("stack"))
				},
			},
			{
				Name:  "passphrase",
				Short: "Manage the secrets passphrase held in the OS vault",
				Long: "Without " + stackconfig.PassphraseEnv + " set, sklp looks the passphrase up in a\n" +
					"vault, so it need not be exported in every shell, CI runner and VM.",
				Run: func(ctx context.Context, args cli.Args) int { return runPassphraseStatus() },
				Sub: []cli.Command{
					{
						Name:  "set",
						Short: "Store the passphrase for this repo in the OS vault",
						Run:   func(ctx context.Context, args cli.Args) int { return runPassphraseSet() },
					},
					{
						Name:  "status",
						Short: "Show where the passphrase comes from and whether it decrypts this repo",
						Run:   func(ctx context.Context, args cli.Args) int { return runPassphraseStatus() },
					},
					{
						Name:  "unset",
						Short: "Remove this repo's passphrase from the OS vault",
						Run:   func(ctx context.Context, args cli.Args) int { return runPassphraseUnset() },
					},
					{
						Name:        "command",
						Short:       "Set the lookup command sklp runs to read the passphrase",
						Long:        "Stores secrets.passphrase_command in the sklp config. {repo} in it is\nreplaced by this repo's name. Pass an empty string to clear it and fall\nback to the OS vault.",
						Positionals: []cli.Positional{{Name: "command"}},
						Run: func(ctx context.Context, args cli.Args) int {
							return runPassphraseCommand(firstArg(args, 0))
						},
					},
				},
			},
		},
	}
}

// configPath is .sklp/<stack>/config.yaml under the cwd repo. Unlike
// layout.Resolve it does NOT require the file to exist (set/import create it),
// but the stack directory must, so a typo doesn't scatter files.
func configPath(stack string) (string, error) {
	if stack == "" {
		return "", fmt.Errorf("a --stack is required")
	}
	dir := filepath.Join(".sklp", stack)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return "", fmt.Errorf("stack %q not found (no %s/)", stack, dir)
	}
	return filepath.Join(dir, "config.yaml"), nil
}

// passphrase resolves the secrets passphrase for the repo in the cwd: the env
// var first, then the vault configured in the sklp config. An unreadable vault
// is reported here rather than surfacing later as a decrypt failure, which
// looks identical to a wrong passphrase.
func passphrase() string {
	v, _, err := stackconfig.ResolvePassphrase(".")
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp config:", err)
		return ""
	}
	return v
}

func runConfigSet(stack, key, value string, secret bool) int {
	if key == "" || value == "" {
		fmt.Fprintln(os.Stderr, "sklp config set: KEY and VALUE are required")
		return 2
	}
	path, err := configPath(stack)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp config:", err)
		return 2
	}
	c, err := stackconfig.Load(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp config:", err)
		return 1
	}
	if secret {
		pp := passphrase()
		if pp == "" {
			fmt.Fprintf(os.Stderr, "sklp config set: --secret needs %s set\n", stackconfig.PassphraseEnv)
			return 1
		}
		if err := c.SetSecret(key, value, pp); err != nil {
			fmt.Fprintln(os.Stderr, "sklp config set:", err)
			return 1
		}
	} else {
		c.SetPlain(key, value)
	}
	if err := stackconfig.Save(path, c); err != nil {
		fmt.Fprintln(os.Stderr, "sklp config set:", err)
		return 1
	}
	kind := "plain"
	if secret {
		kind = "secret"
	}
	fmt.Printf("set %s (%s) in %s\n", key, kind, path)
	return 0
}

func runConfigGet(stack, key string, show bool) int {
	path, err := configPath(stack)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp config:", err)
		return 2
	}
	c, err := stackconfig.Load(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp config:", err)
		return 1
	}
	if v, ok := c.Plain[key]; ok {
		fmt.Println(v)
		return 0
	}
	if !c.IsSecret(key) {
		fmt.Fprintf(os.Stderr, "sklp config get: no such key %q\n", key)
		return 1
	}
	if !show {
		fmt.Println("<secret> (use --show to decrypt)")
		return 0
	}
	resolved, err := c.Resolve(passphrase())
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp config get:", err)
		return 1
	}
	fmt.Println(resolved[key])
	return 0
}

func runConfigList(stack string) int {
	path, err := configPath(stack)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp config:", err)
		return 2
	}
	c, err := stackconfig.Load(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp config:", err)
		return 1
	}
	keys := c.Keys()
	if len(keys) == 0 {
		fmt.Printf("no config in %s\n", path)
		return 0
	}
	for _, k := range keys {
		if c.IsSecret(k) {
			fmt.Printf("%s = <secret>\n", k)
		} else {
			fmt.Printf("%s = %s\n", k, c.Plain[k])
		}
	}
	return 0
}

func runConfigRm(stack, key string) int {
	path, err := configPath(stack)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp config:", err)
		return 2
	}
	c, err := stackconfig.Load(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp config:", err)
		return 1
	}
	if !c.Delete(key) {
		fmt.Fprintf(os.Stderr, "sklp config rm: no such key %q\n", key)
		return 1
	}
	if err := stackconfig.Save(path, c); err != nil {
		fmt.Fprintln(os.Stderr, "sklp config rm:", err)
		return 1
	}
	fmt.Printf("removed %s from %s\n", key, path)
	return 0
}

// runConfigRotate re-seals every secret under the passphrase in
// NewPassphraseEnv, with a fresh salt, and writes the result only once the
// whole set re-encrypted — a failure halfway leaves the file as it was.
//
// Rotating is the only way to change the passphrase: `set` reuses the existing
// salt, so re-entering values by hand would swap the passphrase while keeping
// half the key material, besides passing every secret through argv and shell
// history.
func runConfigRotate(stack string) int {
	path, err := configPath(stack)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp config:", err)
		return 2
	}
	oldPP, newPP := passphrase(), os.Getenv(stackconfig.NewPassphraseEnv)
	if oldPP == "" {
		fmt.Fprintf(os.Stderr, "sklp config rotate: %s must hold the CURRENT passphrase\n", stackconfig.PassphraseEnv)
		return 2
	}
	if newPP == "" {
		fmt.Fprintf(os.Stderr, "sklp config rotate: %s must hold the NEW passphrase\n", stackconfig.NewPassphraseEnv)
		return 2
	}

	c, err := stackconfig.Load(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp config rotate:", err)
		return 1
	}
	sealed := 0
	for _, k := range c.Keys() {
		if c.IsSecret(k) {
			sealed++
		}
	}
	if sealed == 0 {
		fmt.Fprintf(os.Stderr, "sklp config rotate: %s holds no secrets — nothing to rotate\n", path)
		return 1
	}

	rotated, err := c.Rotate(oldPP, newPP)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp config rotate:", err)
		return 1
	}
	if err := stackconfig.Save(path, rotated); err != nil {
		fmt.Fprintln(os.Stderr, "sklp config rotate:", err)
		return 1
	}
	fmt.Printf("rotated %d secret(s) in %s (new salt, %d KDF rounds)\n", sealed, path, rotated.KDFIters())
	fmt.Printf("update %s everywhere it is set: your shell, CI, and any deploy target\n", stackconfig.PassphraseEnv)
	return 0
}

// runConfigImport bulk-loads a dotenv file. --secret encrypts every value (the
// intended use for a .secret file); without it, values are stored plain (a
// .config file). Existing keys are overwritten. Comments (#) and blank lines
// are skipped; only the braced KEY=VALUE form is read (values are taken
// verbatim, quotes stripped).
func runConfigImport(stack, file string, secret bool) int {
	if file == "" {
		fmt.Fprintln(os.Stderr, "sklp config import: a FILE is required")
		return 2
	}
	path, err := configPath(stack)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp config:", err)
		return 2
	}
	f, err := os.Open(file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp config import:", err)
		return 1
	}
	defer f.Close()

	c, err := stackconfig.Load(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp config:", err)
		return 1
	}
	pp := passphrase()
	if secret && pp == "" {
		fmt.Fprintf(os.Stderr, "sklp config import: --secret needs %s set\n", stackconfig.PassphraseEnv)
		return 1
	}

	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if k == "" {
			continue
		}
		if secret {
			if err := c.SetSecret(k, v, pp); err != nil {
				fmt.Fprintln(os.Stderr, "sklp config import:", err)
				return 1
			}
		} else {
			c.SetPlain(k, v)
		}
		n++
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "sklp config import:", err)
		return 1
	}
	if err := stackconfig.Save(path, c); err != nil {
		fmt.Fprintln(os.Stderr, "sklp config import:", err)
		return 1
	}
	kind := "plain"
	if secret {
		kind = "secret"
	}
	fmt.Printf("imported %d %s value(s) from %s into %s\n", n, kind, file, path)
	return 0
}
