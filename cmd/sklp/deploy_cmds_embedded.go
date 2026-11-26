// deploy_cmds_embedded.go holds the read-only deploy sub-verbs `sklp deploy ls`
// and `sklp deploy logs <svc>`. Both are CLIENTS of the per-space control
// socket the running `sklp deploy` supervisor serves: they report the
// SUPERVISOR's truth, not the runc state or the cwd manifest. This is why they
// give the same answer from ANY directory — they never read deploy.yaml.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
	"golang.org/x/sys/unix"
)

// deployLsCommand declares `sklp deploy ls`: the supervisor's view of running
// instances. The legacy -file/-repo flags are accepted (and ignored) for
// backward compatibility; only -space (and the net flags) matter now.
func deployLsCommand() cli.Command {
	return cli.Command{
		Name:  "ls",
		Short: "List running instances of a deploy",
		Flags: append([]cli.Flag{
			{Name: "file", Type: cli.StringFlag, Usage: "(deprecated, ignored) ls queries the live supervisor"},
			{Name: "repo", Type: cli.StringFlag, Usage: "(deprecated, ignored) ls queries the live supervisor"},
			{Name: "space", Type: cli.StringFlag, Usage: "space to inspect (empty = every live space on this host)"},
			jsonFlag,
		}, netFlagGroup()...),
		Run: func(ctx context.Context, args cli.Args) int {
			if args.Bool("json") {
				return runDeployPsJSON(context.Background(), args.String("space"), netFlagsFromArgs(args))
			}
			return runDeployPs(args.String("space"), netFlagsFromArgs(args))
		},
	}
}

// runDeployPs prints the supervisor's view of a space: one row per instance with
// its service, instance id, and state. It asks the control socket — never the
// runc state or deploy.yaml — so it is consistent from any directory and is
// honest when no supervisor is up. Returns the process exit code.
func runDeployPs(space string, nf *netFlags) int {
	// No -space (and no remote --server): fan out across every live space on the
	// host — the multi-space view. A remote target or an explicit -space keeps the
	// single-space path (a remote host serves one space per endpoint today).
	if space == "" && !nf.remote() {
		return runDeployPsAll()
	}
	target := space
	if target == "" {
		target = "production"
	}

	client, err := nf.dialClient(target)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp deploy ls:", err)
		return 1
	}

	ctx := context.Background()
	state, err := client.State(ctx)
	if err != nil {
		reportControlErr("sklp deploy ls", target, err)
		return 1
	}

	if len(state.Services) == 0 {
		fmt.Printf("space %q has no services\n", target)
		return 0
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "SPACE\tSERVICE\tINSTANCE\tSTATUS")
	writeSpaceState(w, target, state)
	w.Flush()
	return 0
}

// runDeployPsAll lists every live space on the host and renders all their
// services in one table — the multi-space ps. Spaces are discovered via the
// per-space control sockets (control.ListSpaces); each is queried over its own
// socket. A space whose socket dies between enumeration and query is skipped
// with a warning, never aborting the whole listing.
func runDeployPsAll() int {
	spaces, err := control.ListSpaces()
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp deploy ls:", err)
		return 1
	}
	if len(spaces) == 0 {
		fmt.Println("no live spaces on this host")
		return 0
	}

	ctx := context.Background()
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "SPACE\tSERVICE\tINSTANCE\tSTATUS")
	for _, sp := range spaces {
		client, err := control.Dial(sp)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sklp deploy ls: space %q: %v\n", sp, err)
			continue
		}
		state, err := client.State(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sklp deploy ls: space %q: %v\n", sp, err)
			continue
		}
		writeSpaceState(w, sp, state)
	}
	w.Flush()
	return 0
}

// writeSpaceState renders one space's services into the shared ps table, with
// the space name in the first column so a multi-space listing is unambiguous.
func writeSpaceState(w *tabwriter.Writer, space string, state control.StateResponse) {
	// Prefer the supervisor's own space name (authoritative) over the caller's
	// --space arg, which may be a default ("production") that differs from the
	// real space ("shop-production"). Falls back to the arg for an old server
	// that does not report state.Space.
	if state.Space != "" {
		space = state.Space
	}
	if len(state.Services) == 0 {
		fmt.Fprintf(w, "%s\t(no services)\t-\t-\n", space)
		return
	}
	for _, svc := range state.Services {
		if len(svc.Instances) == 0 {
			fmt.Fprintf(w, "%s\t%s\t-\t%s\n", space, svc.Name, svc.State)
			continue
		}
		for _, inst := range svc.Instances {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", space, svc.Name, inst.ID, inst.State)
		}
	}
}

// runDeployGet prints one deploy's detail from its supervisor: the space, its
// readiness, and a per-instance table — the single-space twin of `deploy ls`.
// It is a CLIENT of the control socket (the supervisor's truth), so it needs a
// space name (positional) or -space. Unlike `space get` (runc-state only), it
// reports the application-level readiness only a live deploy has.
// deployGetCommand declares `sklp deploy get <space>`: one deploy's detail from
// its supervisor (readiness + per-instance table). The space may be given as a
// positional or via -space.
func deployGetCommand() cli.Command {
	return cli.Command{
		Name:        "get",
		Short:       "Show one deploy's detail (readiness + instances)",
		Positionals: []cli.Positional{{Name: "space", Complete: "space"}},
		Flags: append([]cli.Flag{
			{Name: "space", Type: cli.StringFlag, Usage: "space to inspect (or pass it as a positional)"},
		}, netFlagGroup()...),
		Run: func(ctx context.Context, args cli.Args) int {
			return runDeployGet(args.String("space"), firstArg(args, 0), netFlagsFromArgs(args))
		},
	}
}

// runDeployGet prints one deploy's detail from its supervisor — the single-space
// twin of `deploy ls`. spaceFlag wins over the positional. Returns the exit code.
func runDeployGet(spaceFlag, positional string, nf *netFlags) int {
	target := spaceFlag
	if target == "" {
		target = positional
	}
	if target == "" {
		fmt.Fprintln(os.Stderr, "sklp deploy get: a space name is required")
		fmt.Fprintln(os.Stderr, "usage: sklp deploy get <space>")
		return 2
	}

	client, err := nf.dialClient(target)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp deploy get:", err)
		return 1
	}
	state, err := client.State(context.Background())
	if err != nil {
		reportControlErr("sklp deploy get", target, err)
		return 1
	}

	fmt.Printf("space:     %s\n", target)
	if len(state.Services) == 0 {
		// AllReady() is vacuously true over an empty service set (nothing to
		// wait on) — accurate for its real callers (a deploy front-end
		// blocking on convergence), but "ready: yes" here reads as "this
		// space is healthy" to an operator checking a space they just
		// deleted, which is the opposite of what an empty space means.
		fmt.Printf("services:  (none)\n")
		return 0
	}
	fmt.Printf("ready:     %s\n", yesNoReady(state.AllReady))
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "SERVICE\tINSTANCE\tSTATUS\tUPTIME\tRESTARTS")
	for _, svc := range state.Services {
		if len(svc.Instances) == 0 {
			fmt.Fprintf(w, "%s\t-\t%s\t-\t-\n", svc.Name, svc.State)
			continue
		}
		for _, inst := range svc.Instances {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
				svc.Name, inst.ID, instanceStatus(inst), uptimeOf(inst), restartsOf(inst))
		}
	}
	w.Flush()
	writeRollouts(os.Stdout, state.Services)
	writeJobRuns(os.Stdout, state.Services)
	return 0
}

// instanceStatus annotates the raw state with what makes it permanent: a job
// that spent its retry cap reads "failed" like one that has only just missed
// once, though nothing will relaunch it.
func instanceStatus(inst control.InstanceState) string {
	if inst.Held {
		return inst.State + " (held)"
	}
	return inst.State
}

// uptimeOf ages the CURRENT run. Short uptime beside a high restart count is
// what a crash loop looks like from a single read.
func uptimeOf(inst control.InstanceState) string {
	if inst.StartedAt == nil {
		if inst.RestartAt == nil {
			return "-"
		}
		if due := time.Until(*inst.RestartAt); due > 0 {
			return "restart in " + roundDur(due)
		}
		return "restart due"
	}
	return roundDur(time.Since(*inst.StartedAt))
}

func restartsOf(inst control.InstanceState) string {
	backoff := roundDur(time.Duration(inst.BackoffMS) * time.Millisecond)
	switch {
	case inst.Restarts == 0 && inst.BackoffMS == 0:
		return "-"
	// A backoff without a completed run is a service failing to start at all:
	// reporting "-" here would hide the one case where nothing has come up yet.
	case inst.Restarts == 0:
		return "0 (backoff " + backoff + ")"
	case inst.BackoffMS > 0:
		return fmt.Sprintf("%d (backoff %s)", inst.Restarts, backoff)
	}
	return strconv.Itoa(inst.Restarts)
}

func roundDur(d time.Duration) string {
	if d < 0 {
		return "0s"
	}
	if d < time.Minute {
		return d.Round(time.Second).String()
	}
	return d.Round(time.Minute).String()
}

// writeRollouts prints the rollouts worth reading: the one in flight, and the
// recent failures. A succeeded rollout is just the current state, so listing
// it would bury the two that mean something.
// rolloutsSinceLastSuccess drops everything up to and including the service's
// last clean roll: a failure older than that describes a version no longer
// running, which is history rather than something to act on.
func rolloutsSinceLastSuccess(rollouts []control.RolloutState) []control.RolloutState {
	start := 0
	for i, r := range rollouts {
		if r.Result == control.RolloutSucceeded {
			start = i + 1
		}
	}
	var out []control.RolloutState
	for _, r := range rollouts[start:] {
		if r.Result != control.RolloutSucceeded {
			out = append(out, r)
		}
	}
	return out
}

func writeRollouts(out io.Writer, services []control.ServiceState) {
	type row struct {
		service string
		r       control.RolloutState
	}
	var rows []row
	for _, svc := range services {
		if svc.RejectedReason != "" {
			fmt.Fprintf(out, "\nFROZEN\n  %s: the node refuses to replay this spec (%s)\n",
				svc.Name, svc.RejectedReason)
		}
		for _, r := range rolloutsSinceLastSuccess(svc.Rollouts) {
			rows = append(rows, row{svc.Name, r})
		}
	}
	if len(rows) == 0 {
		return
	}
	fmt.Fprintln(out, "\nROLLOUTS")
	w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "SERVICE\tRESULT\tAGE\tIMAGE\tREASON")
	for _, row := range rows {
		age := "-"
		if !row.r.StartedAt.IsZero() {
			age = roundDur(time.Since(row.r.StartedAt))
		}
		reason := row.r.Reason
		if reason == "" {
			reason = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			row.service, row.r.Result, age, row.r.ToImage, reason)
	}
	w.Flush()
}

const jobRunsShown = 3

func writeJobRuns(out io.Writer, services []control.ServiceState) {
	for _, svc := range services {
		if len(svc.Runs) == 0 {
			continue
		}
		fmt.Fprintf(out, "\nJOB RUNS %s\n", svc.Name)
		runs := svc.Runs
		if len(runs) > jobRunsShown {
			runs = runs[len(runs)-jobRunsShown:]
		}
		w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "AGE\tEXIT\tTRIGGER\tIMAGE")
		for i := len(runs) - 1; i >= 0; i-- {
			r := runs[i]
			trigger := r.Trigger
			if trigger == "" {
				trigger = "-"
			}
			fmt.Fprintf(w, "%s\t%d\t%s\t%s\n", roundDur(time.Since(r.FinishedAt)), r.ExitCode, trigger, r.Image)
		}
		w.Flush()
		last := runs[len(runs)-1]
		for _, line := range last.LogTail {
			fmt.Fprintf(out, "  | %s\n", line)
		}
	}
}

// yesNoReady renders the AllReady flag for `deploy get`.
func yesNoReady(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// runDeployLogs streams (or follows with -f) one service's captured logs from
// the supervisor over the control socket. It does NOT read deploy.yaml or the
// log cache directly, so it works from any directory and an unknown service is
// reported by the server (404). Ctrl-C in follow mode cancels the request
// context; the server stops its followers — no leaked tail.
// deployLogsCommand declares `sklp deploy logs <service> [-f]`. The legacy
// -file/-repo flags are accepted (and ignored); -f aliases -follow. The service
// positional may sit before/among/after flags.
func deployLogsCommand() cli.Command {
	return cli.Command{
		Name:        "logs",
		Short:       "Stream (or follow with -f) a service's container output",
		Positionals: []cli.Positional{{Name: "service", Required: true}},
		Flags: append([]cli.Flag{
			{Name: "file", Type: cli.StringFlag, Usage: "(deprecated, ignored) logs stream from the live supervisor"},
			{Name: "repo", Type: cli.StringFlag, Usage: "(deprecated, ignored) logs stream from the live supervisor"},
			{Name: "space", Type: cli.StringFlag, StringDefault: "production", Usage: "space the service belongs to"},
			{Name: "follow", Aliases: []string{"f"}, Type: cli.BoolFlag, Usage: "follow the log (tail -f); Ctrl-C to stop"},
		}, netFlagGroup()...),
		Run: func(ctx context.Context, args cli.Args) int {
			return runDeployLogs(firstArg(args, 0), args.String("space"), args.Bool("follow"), netFlagsFromArgs(args))
		},
	}
}

// runDeployLogs streams (or follows) one service's captured logs from the
// supervisor over the control socket. It does NOT read deploy.yaml or the log
// cache directly, so it works from any directory and an unknown service is
// reported by the server (404). Ctrl-C in follow mode cancels the request
// context; the server stops its followers — no leaked tail. Returns the exit code.
func runDeployLogs(service, space string, follow bool, nf *netFlags) int {
	if service == "" {
		fmt.Fprintln(os.Stderr, "sklp deploy logs: exactly one service name is required")
		fmt.Fprintln(os.Stderr, "usage: sklp deploy logs <service> [-f] [-space X]")
		return 2
	}

	client, err := nf.dialClient(space)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp deploy logs:", err)
		return 1
	}

	// Follow mode binds to a signal-cancelled context so Ctrl-C cuts the request
	// (and the server-side follower) cleanly.
	ctx, stop := signal.NotifyContext(context.Background(), unix.SIGINT, unix.SIGTERM)
	defer stop()

	if err := client.Logs(ctx, service, follow, os.Stdout); err != nil {
		reportControlErr("sklp deploy logs", space, err)
		return 1
	}
	return 0
}

// reportControlErr prints an honest message: a missing/refused socket means the
// space is not deployed (no supervisor); any other error passes through.
func reportControlErr(prefix, space string, err error) {
	if errors.Is(err, control.ErrNoSupervisor) {
		fmt.Fprintf(os.Stderr, "%s: space %q is not deployed (no supervisor running)\n", prefix, space)
		return
	}
	fmt.Fprintln(os.Stderr, prefix+":", err)
}

type spaceStateJSON struct {
	Space string `json:"space"`
	State any    `json:"state,omitempty"`
	Error string `json:"error,omitempty"`
}

func runDeployPsJSON(ctx context.Context, space string, nf *netFlags) int {
	spaces := []string{space}
	if space == "" {
		var err error
		if spaces, err = nodeSpaces(ctx, nf); err != nil {
			fmt.Fprintln(os.Stderr, "sklp deploy ls:", err)
			return 1
		}
	}
	out := make([]spaceStateJSON, 0, len(spaces))
	for _, sp := range spaces {
		row := spaceStateJSON{Space: sp}
		client, err := nf.dialClient(sp)
		if err == nil {
			var st any
			if st, err = client.State(ctx); err == nil {
				row.State = st
			}
		}
		if err != nil {
			row.Error = err.Error()
		}
		out = append(out, row)
	}
	return writeJSON(os.Stdout, "sklp deploy ls", out)
}
