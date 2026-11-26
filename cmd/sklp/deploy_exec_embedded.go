// deploy_exec_embedded.go holds the `sklp deploy exec <svc> [-- cmd...]` and
// `sklp deploy restart <svc>` sub-verbs. Both are CLIENTS of the per-space
// control socket: exec runs a command inside a live instance (docker/kubectl
// exec, interactive TTY by default), restart bounces a service's instances.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/lalternativefabrique/sklp/internal/cli"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// deployExecCommand declares `sklp deploy exec`. It is RawArgs because its
// docker/kubectl-style tail (`exec web ls -la`, where the command after the
// service is verbatim and may carry its OWN dashes WITHOUT a `--`) cannot be
// expressed by the façade's flag/positional model: the façade would reject `-la`
// as an unknown flag. The flag-free parseExecArgs reproduces the legacy semantics
// over the raw tail — no flag.NewFlagSet involved.
func deployExecCommand() cli.Command {
	return cli.Command{
		Name:    "exec",
		Short:   "Run a command (or open a shell) inside a live instance",
		RawArgs: true,
		Run: func(ctx context.Context, args cli.Args) int {
			return runDeployExec(args.Passthrough())
		},
	}
}

// execKnownFlags lists the string flags exec recognises around the service token
// (everything from -space + the net flag group). A bare -insecure is a bool flag
// and takes no value. Used by the flag-free parser to know which `-flag VALUE`
// pairs consume two tokens.
var execKnownStringFlags = map[string]bool{
	"space": true, "server": true, "addr": true, "token": true,
	"token-file": true, "ca-fingerprint": true,
}

// runDeployExec runs a command inside a live instance of a service. Usage:
//
//	sklp deploy exec [-space X] <svc> [cmd args...]
//	sklp deploy exec [-space X] <svc> -- cmd args...
//
// Docker/kubectl-style parsing (see parseExecArgs): known flags are recognised
// wherever they precede the service; the first non-flag token is the service;
// everything AFTER it is the command (no `--` required). The default command is
// `sh`, so `exec web` opens a shell. When stdin AND stdout are a terminal it
// requests an interactive PTY. Returns the process exit code.
func runDeployExec(args []string) int {
	flags, service, cmd, perr := parseExecArgs(args)
	if perr != nil || service == "" {
		if perr != nil {
			fmt.Fprintln(os.Stderr, "sklp deploy exec:", perr)
		} else {
			fmt.Fprintln(os.Stderr, "sklp deploy exec: a service name is required")
		}
		fmt.Fprintln(os.Stderr, "usage: sklp deploy exec [-space X] <service> [cmd args...]")
		return 2
	}
	if len(cmd) == 0 {
		cmd = []string{"sh"} // default to an interactive shell, like `docker exec`
	}
	space := flags.space
	if space == "" {
		space = "production"
	}
	nf := flags.netFlags

	client, err := nf.dialClient(space)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp deploy exec:", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), unix.SIGINT, unix.SIGTERM)
	defer stop()

	tty := isExecTerminal()
	var rows, cols uint16
	var restore func()
	if tty {
		// Raw mode so keystrokes (including Ctrl-C, arrows) flow to the container
		// instead of the local shell; restored on exit no matter how we leave.
		fd := int(os.Stdin.Fd())
		if st, terr := term.MakeRaw(fd); terr == nil {
			restore = func() { _ = term.Restore(fd, st) }
			defer restore()
		}
		if w, h, serr := term.GetSize(int(os.Stdout.Fd())); serr == nil {
			cols, rows = uint16(w), uint16(h)
		}
	}

	code, err := client.Exec(ctx, service, cmd, tty, rows, cols, os.Stdin, os.Stdout, os.Stderr)
	if restore != nil {
		restore()
	}
	if err != nil {
		reportControlErr("sklp deploy exec", space, err)
		return 1
	}
	return code
}

// execFlags holds the flags exec recognises around its service positional.
type execFlags struct {
	space string
	*netFlags
}

// parseExecArgs parses `exec [flags] <svc> [cmd...]` the way docker/kubectl do,
// so `exec web sh`, `exec web -- sh`, `exec web ls -la`, `exec web` and
// `exec -space prod web sh` all work. Rules:
//
//   - An explicit `--` is the hard split: flags + the (single) service before;
//     everything after is the LITERAL command (its flags are not interpreted).
//   - Without `--`: the FIRST non-flag token is the service. Known flags may sit
//     on EITHER side of it; the command is the run of tokens starting at the
//     first non-flag AFTER the service — taken verbatim. A command whose first
//     token starts with `-` needs the explicit `--` form.
//
// It is flag-free (no flag.NewFlagSet): it walks tokens recognising the known
// -space / net flags by name, matching the legacy flag.FlagSet behaviour.
func parseExecArgs(args []string) (flags execFlags, service string, cmd []string, err error) {
	flags.netFlags = &netFlags{}

	// Hard split on the first bare "--": flags+service before, literal command
	// after.
	if pre, post, ok := splitAtDoubleDash(args); ok {
		svc, perr := parseExecHead(&flags, pre)
		if perr != nil {
			return flags, "", nil, perr
		}
		return flags, svc, post, nil
	}

	// No "--": consume flags up to the first non-flag (the service), then keep
	// consuming flags after it until the first non-flag — that token onward is the
	// verbatim command.
	rest, perr := consumeExecFlags(&flags, args, true)
	if perr != nil {
		return flags, "", nil, perr
	}
	if len(rest) == 0 {
		return flags, "", nil, nil
	}
	service = rest[0]
	tail, perr := consumeExecFlags(&flags, rest[1:], false)
	if perr != nil {
		return flags, "", nil, perr
	}
	return flags, service, tail, nil
}

// parseExecHead parses the pre-"--" segment: flags + EXACTLY one positional (the
// service) placed anywhere among them, rejecting extras.
func parseExecHead(flags *execFlags, args []string) (service string, err error) {
	rest, perr := consumeExecFlags(flags, args, true)
	if perr != nil {
		return "", perr
	}
	if len(rest) == 0 {
		return "", nil
	}
	service = rest[0]
	tail, perr := consumeExecFlags(flags, rest[1:], true)
	if perr != nil {
		return "", perr
	}
	if len(tail) > 0 {
		return "", fmt.Errorf("unexpected extra argument %q", tail[0])
	}
	return service, nil
}

// consumeExecFlags walks args setting recognised flags on flags. When
// stopAtArg is true it stops at (and returns from) the first non-flag token;
// otherwise it also stops there (the command run). An unknown flag is an error
// only while stopAtArg is true (before the command); after the service the
// command is verbatim so any token is returned as-is.
func consumeExecFlags(flags *execFlags, args []string, stopAtArg bool) (rest []string, err error) {
	i := 0
	for i < len(args) {
		a := args[i]
		if len(a) == 0 || a[0] != '-' || a == "-" {
			return args[i:], nil
		}
		name, val, hasEq := splitFlag(a)
		switch {
		case name == "insecure":
			flags.insecure = true
			i++
			continue
		case execKnownStringFlags[name]:
			v := val
			if !hasEq {
				if i+1 >= len(args) {
					return nil, fmt.Errorf("flag -%s needs a value", name)
				}
				v = args[i+1]
				i += 2
			} else {
				i++
			}
			setExecFlag(flags, name, v)
			continue
		default:
			if stopAtArg {
				return nil, fmt.Errorf("unknown flag -%s", name)
			}
			return args[i:], nil
		}
	}
	return nil, nil
}

// setExecFlag stores a recognised string flag value (server/addr alias collapse).
func setExecFlag(flags *execFlags, name, v string) {
	switch name {
	case "space":
		flags.space = v
	case "server", "addr":
		flags.server = v
	case "token":
		flags.token = v
	case "token-file":
		flags.tokenFile = v
	case "ca-fingerprint":
		flags.caFinger = v
	}
}

// splitAtDoubleDash splits args at the first bare "--": before/after the marker,
// with ok=true when a "--" was present. With no "--", ok is false.
func splitAtDoubleDash(args []string) (before, after []string, ok bool) {
	for i, a := range args {
		if a == "--" {
			return args[:i], args[i+1:], true
		}
	}
	return args, nil, false
}

// isExecTerminal reports whether both stdin and stdout are TTYs, so exec only
// requests an interactive PTY when the user is actually at a terminal (a piped
// `exec web -- cmd <input` stays non-interactive).
var isExecTerminal = func() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

// deployRestartCommand declares `sklp deploy restart <service>`. The service
// positional may sit before/among/after the flags (façade interleaving).
func deployRestartCommand() cli.Command {
	return cli.Command{
		Name:        "restart",
		Short:       "Bounce every live instance of a service",
		Positionals: []cli.Positional{{Name: "service", Required: true}},
		Flags: append([]cli.Flag{
			{Name: "space", Type: cli.StringFlag, StringDefault: "production", Usage: "space the service belongs to"},
		}, netFlagGroup()...),
		Run: func(ctx context.Context, args cli.Args) int {
			return runDeployRestart(firstArg(args, 0), args.String("space"), netFlagsFromArgs(args))
		},
	}
}

// runDeployRestart bounces every live instance of a service over the control
// socket. Returns the process exit code.
func runDeployRestart(service, space string, nf *netFlags) int {
	if service == "" {
		fmt.Fprintln(os.Stderr, "sklp deploy restart: exactly one service name is required")
		fmt.Fprintln(os.Stderr, "usage: sklp deploy restart <service> [-space X]")
		return 2
	}

	client, err := nf.dialClient(space)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp deploy restart:", err)
		return 1
	}
	if err := client.Restart(context.Background(), service); err != nil {
		reportControlErr("sklp deploy restart", space, err)
		return 1
	}
	fmt.Printf("service %q: restart requested\n", service)
	return 0
}
