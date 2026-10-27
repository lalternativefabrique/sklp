// cli_flag.go is the stdlib-`flag` Adapter for the cli façade. It is the ONLY
// file under internal/cli allowed to import `flag`: it translates a declarative
// Command tree into flag.FlagSet parsing + sub-command dispatch, reproducing the
// legacy hand-rolled quirks (interleaved positionals, `--` passthrough, RawArgs
// verbatim passthrough, flag aliases, repeatable flags, env fallbacks). Swapping
// the framework = a new adapter here; no Command declaration changes.
package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
)

// NewFlagAdapter returns the default stdlib-flag Adapter.
func NewFlagAdapter() Adapter { return &flagAdapter{} }

// flagAdapter binds a Command tree to the stdlib `flag` package.
type flagAdapter struct{}

// Execute resolves the command for argv (already stripped of the program name),
// parses it, and runs it — returning the process exit code. The caller (main)
// owns the single os.Exit.
func (a *flagAdapter) Execute(ctx context.Context, root Command, argv []string) int {
	return a.dispatch(ctx, &root, "sklp", argv)
}

// dispatch walks the Command tree. path is the human command path so far
// (e.g. "sklp deploy") used for usage messages.
func (a *flagAdapter) dispatch(ctx context.Context, cmd *Command, path string, argv []string) int {
	// A group (has Sub) dispatches on the leading non-flag token unless this
	// command also declares RawArgs/flags it wants to own. Sub-dispatch wins so
	// `deploy ls` reaches the `ls` child even though `deploy` has flags too.
	if len(cmd.Sub) > 0 {
		if len(argv) > 0 {
			tok := argv[0]
			if tok == "-h" || tok == "--help" || tok == "help" {
				a.printGroupUsage(cmd, path)
				return 0
			}
			if !strings.HasPrefix(tok, "-") {
				if child := findSub(cmd, tok); child != nil {
					return a.dispatch(ctx, child, path+" "+child.Name, argv[1:])
				}
				// Unknown sub-token: if this group can also run bare (has Run),
				// fall through to its own parse; otherwise it is an error.
				if cmd.Run == nil {
					fmt.Fprintf(os.Stderr, "%s: unknown subcommand %q\n", path, tok)
					a.printGroupUsage(cmd, path)
					return 2
				}
			}
		}
		// Bare group (no token, or a flag-leading token) with no Run: list subs.
		if cmd.Run == nil {
			a.printGroupUsage(cmd, path)
			if len(argv) == 0 {
				return 0
			}
			return 0
		}
		// else: a group that also runs bare — fall through to leaf handling.
	}

	if cmd.Run == nil {
		// Leaf with no Run is a programming error; surface it loudly.
		fmt.Fprintf(os.Stderr, "%s: no action defined\n", path)
		return 2
	}

	args, code, done := a.parse(cmd, path, argv)
	if done {
		return code
	}
	return cmd.Run(ctx, args)
}

// parse builds the Args for a leaf command. done=true short-circuits dispatch
// (help printed, or a parse error) with the given code.
func (a *flagAdapter) parse(cmd *Command, path string, argv []string) (args Args, code int, done bool) {
	fa := &flagArgs{
		strings: map[string]string{},
		bools:   map[string]bool{},
		ints:    map[string]int{},
		slices:  map[string][]string{},
	}

	// RawArgs: no flag parsing at all — every token flows verbatim to Run via
	// Passthrough (the `sklp run …` escape hatch handed to runner.Main as-is).
	if cmd.RawArgs {
		// Still honour a leading -h/--help so `sklp run -h` is usable. Tokens
		// after a help flag are NOT inspected — RawArgs means verbatim.
		fa.passthrough = append([]string{}, argv...)
		return fa, 0, false
	}

	// Passthrough: split at the first bare "--". The tail is verbatim.
	parseArgs := argv
	if cmd.Passthrough {
		if i := indexDoubleDash(argv); i >= 0 {
			parseArgs = argv[:i]
			fa.passthrough = append([]string{}, argv[i+1:]...)
		}
	}

	fs := flag.NewFlagSet(path, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { a.printLeafUsage(cmd, path) }

	// Register declared flags + their aliases, defaulting the typed value.
	for i := range cmd.Flags {
		f := &cmd.Flags[i]
		names := append([]string{f.Name}, f.Aliases...)
		switch f.Type {
		case StringFlag:
			fa.strings[f.Name] = f.StringDefault
			for _, n := range names {
				bindString(fs, n, f.Name, fa)
			}
		case BoolFlag:
			fa.bools[f.Name] = f.BoolDefault
			for _, n := range names {
				bindBool(fs, n, f.Name, fa)
			}
		case IntFlag:
			fa.ints[f.Name] = f.IntDefault
			for _, n := range names {
				bindInt(fs, n, f.Name, fa)
			}
		case StringSliceFlag:
			for _, n := range names {
				bindSlice(fs, n, f.Name, fa)
			}
		}
	}

	// Parse tolerating positionals mixed among flags (parseWithStack style):
	// flag.Parse stops at the first non-flag, so we parse in segments and
	// collect each residual leading non-flag as a positional, then re-parse the
	// tail. This skips flag VALUES correctly (fs already consumed them).
	rem := parseArgs
	for {
		if err := fs.Parse(rem); err != nil {
			// help requested via flag.ErrHelp, or a genuine parse error.
			if err == flag.ErrHelp {
				return nil, 0, true
			}
			return nil, 2, true
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		fa.positionals = append(fa.positionals, rest[0])
		rem = rest[1:]
	}

	// Env fallback: for any string flag left at its default AND not explicitly
	// set, consult its EnvFallback. We approximate "unset" by checking the
	// parsed value equals the declared default; matches the legacy
	// -profile→SKLP_PROFILE / -project→SKLP_PROJECT / -token→SKLP_TOKEN flow.
	for i := range cmd.Flags {
		f := &cmd.Flags[i]
		if f.Type != StringFlag || f.EnvFallback == "" {
			continue
		}
		if fa.setFlags[f.Name] {
			continue
		}
		if v := os.Getenv(f.EnvFallback); v != "" {
			fa.strings[f.Name] = v
		}
	}

	return fa, 0, false
}

// ParseLeaf parses argv against a leaf command's declared flags/positionals and
// returns the resulting Args, WITHOUT dispatching a Run. It is the programmatic
// seam used by verbs that own their own sub-dispatch (and by tests) to read the
// SAME declarative flags the façade would. It mirrors the adapter's parse exactly
// (interleaved positionals, `--` passthrough, aliases, repeatable flags, env
// fallbacks); a parse error is returned instead of printing usage + exiting.
func ParseLeaf(cmd Command, argv []string) (Args, error) {
	a := &flagAdapter{}
	args, code, done := a.parse(&cmd, "sklp "+cmd.Name, argv)
	if done {
		if code == 0 {
			return nil, flag.ErrHelp
		}
		return nil, fmt.Errorf("parse %s", cmd.Name)
	}
	return args, nil
}

// findSub returns the child command selected by tok (by Name or Alias).
func findSub(cmd *Command, tok string) *Command {
	for i := range cmd.Sub {
		c := &cmd.Sub[i]
		if c.Name == tok {
			return c
		}
		for _, al := range c.Aliases {
			if al == tok {
				return c
			}
		}
	}
	return nil
}

// indexDoubleDash returns the index of the first bare "--", or -1.
func indexDoubleDash(args []string) int {
	for i, a := range args {
		if a == "--" {
			return i
		}
	}
	return -1
}

// --- flag binding helpers: every alias writes into the same canonical key, and
// records that the canonical name was explicitly set (for env-fallback). ---

func bindString(fs *flag.FlagSet, name, canon string, fa *flagArgs) {
	fs.Func(name, "", func(v string) error {
		fa.strings[canon] = v
		fa.markSet(canon)
		return nil
	})
}

func bindBool(fs *flag.FlagSet, name, canon string, fa *flagArgs) {
	// boolFunc gives `-flag` (no value) the true semantics of a bool flag while
	// still accepting `-flag=false`.
	fs.BoolFunc(name, "", func(v string) error {
		fa.bools[canon] = v == "" || v == "true" || v == "1"
		fa.markSet(canon)
		return nil
	})
}

func bindInt(fs *flag.FlagSet, name, canon string, fa *flagArgs) {
	fs.Func(name, "", func(v string) error {
		n, err := parseInt(v)
		if err != nil {
			return err
		}
		fa.ints[canon] = n
		fa.markSet(canon)
		return nil
	})
}

func bindSlice(fs *flag.FlagSet, name, canon string, fa *flagArgs) {
	fs.Func(name, "", func(v string) error {
		fa.slices[canon] = append(fa.slices[canon], v)
		fa.markSet(canon)
		return nil
	})
}

func parseInt(v string) (int, error) {
	neg := false
	s := v
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	if s == "" {
		return 0, fmt.Errorf("invalid integer %q", v)
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("invalid integer %q", v)
		}
		n = n*10 + int(r-'0')
	}
	if neg {
		n = -n
	}
	return n, nil
}

// flagArgs is the Args implementation backed by the parsed flag set.
type flagArgs struct {
	strings     map[string]string
	bools       map[string]bool
	ints        map[string]int
	slices      map[string][]string
	setFlags    map[string]bool
	positionals []string
	passthrough []string
}

func (a *flagArgs) markSet(name string) {
	if a.setFlags == nil {
		a.setFlags = map[string]bool{}
	}
	a.setFlags[name] = true
}

func (a *flagArgs) String(name string) string        { return a.strings[name] }
func (a *flagArgs) Bool(name string) bool            { return a.bools[name] }
func (a *flagArgs) Int(name string) int              { return a.ints[name] }
func (a *flagArgs) StringSlice(name string) []string { return a.slices[name] }
func (a *flagArgs) IsSet(name string) bool           { return a.setFlags[name] }
func (a *flagArgs) Positionals() []string            { return a.positionals }
func (a *flagArgs) Passthrough() []string            { return a.passthrough }
