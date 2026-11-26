// deploy_remote_net.go is the NON-gated network-pilot path: it lets a binary
// built WITHOUT the embedded runtime drive a remote supervisor over the control
// API (--server + token + TLS), the same way deploy_remote.go lets it pilot over
// ssh. It intercepts the read/control sub-verbs (ps/logs/exec/restart/delete) when
// --server is present and runs them via the network client; the SERVER (which
// needs the embedded runtime) does the actual work.
//
// On an embedded build the gated sub-verbs already handle --server themselves, so
// this interceptor only fires for sub-verbs and only when --server is present —
// it never shadows the local (socket) path or the plain deploy flow.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"text/tabwriter"
	"time"

	"github.com/lalternativefabrique/sklp/spacemanager/control"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// maybeRunRemoteNet intercepts a deploy sub-verb (ps/logs/exec/restart/delete) when
// --server/--addr is present, runs it over the network control client, and
// returns handled=true with the process exit code. Without --server, or for the
// plain deploy / token / fingerprint verbs, it returns handled=false so the normal
// (gated or stub) flow runs. It is called from main.go AFTER the ssh interceptor.
func maybeRunRemoteNet(args []string) (handled bool, exitCode int) {
	if len(args) == 0 || len(args[0]) == 0 || args[0][0] == '-' {
		return false, 0 // plain deploy (flags first) is not a network sub-verb
	}
	verb := args[0]
	switch verb {
	case "ls", "logs", "exec", "restart", "delete":
	default:
		return false, 0 // token/fingerprint/__supervise/unknown: not for the net client
	}
	// Peek: only take over when --server/--addr is actually present.
	if !hasServerFlag(args[1:]) {
		return false, 0
	}
	switch verb {
	case "ls":
		return true, runRemoteNetPs(args[1:])
	case "logs":
		return true, runRemoteNetLogs(args[1:])
	case "restart":
		return true, runRemoteNetRestart(args[1:])
	case "delete":
		return true, runRemoteNetDown(args[1:])
	case "exec":
		return true, runRemoteNetExec(args[1:])
	}
	return false, 0
}

// hasServerFlag reports whether --server/--addr (or =form) appears in args.
func hasServerFlag(args []string) bool {
	for _, a := range args {
		name, _, _ := splitFlag(a)
		if name == "server" || name == "addr" {
			return true
		}
	}
	return false
}

func runRemoteNetPs(args []string) int {
	fs := flag.NewFlagSet("deploy ls", flag.ExitOnError)
	space := fs.String("space", "production", "space to inspect")
	nf := bindNetFlags(fs)
	fs.Parse(args)

	client, err := nf.dialClient(*space)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp deploy ls:", err)
		return 1
	}
	state, err := client.State(context.Background())
	if err != nil {
		reportNetErr("sklp deploy ls", *space, err)
		return 1
	}
	if len(state.Services) == 0 {
		fmt.Printf("space %q has no services\n", *space)
		return 0
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "SERVICE\tINSTANCE\tSTATUS")
	for _, svc := range state.Services {
		if len(svc.Instances) == 0 {
			fmt.Fprintf(w, "%s\t-\t%s\n", svc.Name, svc.State)
			continue
		}
		for _, inst := range svc.Instances {
			fmt.Fprintf(w, "%s\t%s\t%s\n", svc.Name, inst.ID, inst.State)
		}
	}
	w.Flush()
	return 0
}

func runRemoteNetLogs(args []string) int {
	fs := flag.NewFlagSet("deploy logs", flag.ExitOnError)
	space := fs.String("space", "production", "space the service belongs to")
	follow := fs.Bool("f", false, "follow the log (tail -f); Ctrl-C to stop")
	fs.BoolVar(follow, "follow", false, "follow the log (tail -f); Ctrl-C to stop")
	nf := bindNetFlags(fs)
	service, perr := parseWithPositional(fs, args)
	if perr != nil || service == "" {
		fmt.Fprintln(os.Stderr, "sklp deploy logs: exactly one service name is required")
		return 2
	}
	client, err := nf.dialClient(*space)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp deploy logs:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), unix.SIGINT, unix.SIGTERM)
	defer stop()
	if err := client.Logs(ctx, service, *follow, os.Stdout); err != nil {
		reportNetErr("sklp deploy logs", *space, err)
		return 1
	}
	return 0
}

func runRemoteNetRestart(args []string) int {
	fs := flag.NewFlagSet("deploy restart", flag.ExitOnError)
	space := fs.String("space", "production", "space the service belongs to")
	nf := bindNetFlags(fs)
	service, perr := parseWithPositional(fs, args)
	if perr != nil || service == "" {
		fmt.Fprintln(os.Stderr, "sklp deploy restart: exactly one service name is required")
		return 2
	}
	client, err := nf.dialClient(*space)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp deploy restart:", err)
		return 1
	}
	if err := client.Restart(context.Background(), service); err != nil {
		reportNetErr("sklp deploy restart", *space, err)
		return 1
	}
	fmt.Printf("service %q: restart requested\n", service)
	return 0
}

func runRemoteNetDown(args []string) int {
	fs := flag.NewFlagSet("deploy delete", flag.ExitOnError)
	space := fs.String("space", "production", "space to tear down")
	volumes := fs.Bool("v", false, "also destroy the space's named volumes — the data goes with it, irreversibly")
	nf := bindNetFlags(fs)
	fs.Parse(args)

	client, err := nf.dialClient(*space)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp deploy delete:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := deleteRemote(ctx, client, *volumes); err != nil {
		reportNetErr("sklp deploy delete", *space, err)
		return 1
	}
	fmt.Println(deleteAck(*space, *volumes))
	return 0
}

func runRemoteNetExec(args []string) int {
	fs := flag.NewFlagSet("deploy exec", flag.ExitOnError)
	space := fs.String("space", "production", "space the service belongs to")
	nf := bindNetFlags(fs)
	pre, cmd := splitDoubleDash(args)
	service, perr := parseWithPositional(fs, pre)
	if perr != nil || service == "" {
		fmt.Fprintln(os.Stderr, "sklp deploy exec: exactly one service name is required")
		return 2
	}
	if len(cmd) == 0 {
		cmd = []string{"sh"}
	}
	client, err := nf.dialClient(*space)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp deploy exec:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), unix.SIGINT, unix.SIGTERM)
	defer stop()

	tty, rows, cols, restore := setupRemoteExecTerminal()
	if restore != nil {
		defer restore()
	}
	code, err := client.Exec(ctx, service, cmd, tty, rows, cols, os.Stdin, os.Stdout, os.Stderr)
	if restore != nil {
		restore()
	}
	if err != nil {
		reportNetErr("sklp deploy exec", *space, err)
		return 1
	}
	return code
}

// setupRemoteExecTerminal puts the local terminal in raw mode when both stdin and
// stdout are TTYs (so keystrokes flow to the container), returning whether a tty
// was requested, the window size, and a restore func (nil when not a tty).
func setupRemoteExecTerminal() (tty bool, rows, cols uint16, restore func()) {
	if !(term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))) {
		return false, 0, 0, nil
	}
	fd := int(os.Stdin.Fd())
	if st, err := term.MakeRaw(fd); err == nil {
		restore = func() { _ = term.Restore(fd, st) }
	}
	if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
		cols, rows = uint16(w), uint16(h)
	}
	return true, rows, cols, restore
}

// reportNetErr prints an honest message for a network control error. A missing/
// refused endpoint surfaces as the underlying transport error (there is no socket
// to map to ErrNoSupervisor on the network side).
func reportNetErr(prefix, space string, err error) {
	if errors.Is(err, control.ErrNoSupervisor) {
		fmt.Fprintf(os.Stderr, "%s: space %q supervisor is unreachable\n", prefix, space)
		return
	}
	fmt.Fprintln(os.Stderr, prefix+":", err)
}
