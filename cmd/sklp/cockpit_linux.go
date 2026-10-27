// cockpit.go wires `sklp cockpit` — the full-screen read-first monitor. The Run
// body is thin transport glue (mirrors flow.go): it builds the initial Model
// from the real state sources, picks a Renderer, and either drives the
// bubbletea App (TTY) or prints a one-shot snapshot via the SAME view.go with a
// plain-text Renderer (non-TTY). All logic lives in package cockpit.
package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lalternativefabrique/sklp/cmd/sklp/internal/flow"
	"github.com/lalternativefabrique/sklp/cockpit"
	"github.com/lalternativefabrique/sklp/devstack"
	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/internal/layout"
	"github.com/lalternativefabrique/sklp/pipeline"
	"github.com/lalternativefabrique/sklp/space/spaceapi"
	"golang.org/x/term"
)

// init wires the read-only seams into the cockpit package. The internal-package
// and embedded-runtime boundaries forbid cockpit from importing flow / spaceapi
// directly, so the command layer injects them.
func init() {
	cockpit.FlowRead = readFlow
	cockpit.SpaceList = listSpaces
	cockpit.RemoteNodeList = listRemoteNodes
	cockpit.ContextList = listContexts
	cockpit.HostRunList = listHostRuns
	cockpit.VMStatRead = readVMState
	cockpit.VMLogStream = streamVMLogs
	cockpit.LogStream = streamLogs
	cockpit.CILogStream = streamCILogs
	cockpit.KillSpace = killSpace
	cockpit.StopSpace = stopSpace
	cockpit.PurgeSpaces = purgeSpaces
	cockpit.Clipboard = copyToClipboard
	cockpit.SpaceExec = execIntoContainer
	cockpit.RestartDev = restartDevStack
}

// restartDevStack re-runs `sklp dev <stack>` for a stopped dev space. There is
// no daemon to signal: `sklp dev` is a foreground supervisor, so restarting is
// running it again — in the operator's terminal, which the cockpit has released
// through tea.Exec by the time this is called. It blocks until the supervisor
// exits (Ctrl-C), then the cockpit comes back.
//
// The stack is recovered by hashing every dev stack of the repo back to the
// space name; an unresolvable space is reported rather than guessed at, since
// launching the wrong stack would start the wrong services.
func restartDevStack(space, repo string) error {
	stack, ok := devstack.StackForSpace(repo, space)
	if !ok {
		return fmt.Errorf("no dev stack of %s maps to space %s — run `sklp dev` there directly", repo, space)
	}
	args := []string{"-repo", repo}
	if stack != "" {
		args = append(args, stack)
	}
	label := stack
	if label == "" {
		label = "(default stack)"
	}
	fmt.Printf("── sklp dev %s in %s — Ctrl-C to stop and return to the cockpit ──\n", label, repo)
	if code := devstack.Main(args); code != 0 {
		return fmt.Errorf("sklp dev exited with code %d", code)
	}
	return nil
}

// listHostRuns enumerates the repo's host-only pipelines (kind: pipeline,
// space: false — e.g. publish). These never create a space, so they would be
// invisible in the Spaces list; the cockpit shows them separately with their
// central run log tailable. Repo is the cockpit's cwd. A non-repo cwd or read
// error yields no rows (the section just doesn't appear).
func listHostRuns() []cockpit.HostRun {
	repo, err := os.Getwd()
	if err != nil {
		return nil
	}
	l := layout.New(repo)
	names, err := l.Pipelines()
	if err != nil {
		return nil
	}
	var out []cockpit.HostRun
	for _, name := range names {
		p, err := pipeline.LoadPipeline(filepath.Join(l.PipelinesDir(), name+".yaml"))
		if err != nil {
			continue
		}
		// space:false (explicit) marks a host-only run. nil/true = sandboxed.
		if p.Space == nil || *p.Space {
			continue
		}
		out = append(out, cockpit.HostRun{
			Name:   name,
			HasLog: pipeline.LatestCILogForTask(repo, name) != "",
		})
	}
	return out
}

// streamCILogs tails the newest CI run-log for one TASK in a repo. A
// non-supervised CI space has no control socket (the runner exits when the run
// converges), but the reporter mirrored its log into the central store as
// ci-<task>-<ts>.log, keyed by repo+task so concurrent pipelines don't collide.
// repo comes from the space's bundle when alive; once torn down the bundle is
// gone, so we fall back to the cockpit's own repo (cwd) — the operator runs the
// cockpit inside the repo whose CI they watch. task is the space name. We emit
// every existing line, then poll for appends until ctx is cancelled.
func streamCILogs(ctx context.Context, repo, task string, onLine func(string)) (string, error) {
	if repo == "" {
		if cwd, err := os.Getwd(); err == nil {
			repo = cwd
		}
	}
	// The cockpit keys this by SPACE name, which only doubles as a task name for
	// CI spaces (ci-<hash>). A dev space has no task of its own, so fall back to
	// the repo's newest run rather than reporting "no log" for a repo that has
	// plenty.
	path := pipeline.LatestCILogForTask(repo, task)
	if path == "" {
		path = pipeline.LatestDevLogForStack(repo, "")
	}
	if path == "" {
		path = pipeline.LatestCILogForRepo(repo)
	}
	if path == "" {
		return "", fmt.Errorf("no run log yet for %s — run `sklp run <task>` or `sklp dev <stack>` in this repo first", pipeline.CentralLogDir(repo))
	}
	onLine("── tailing " + path + " ──")
	f, err := os.Open(path)
	if err != nil {
		return path, err
	}
	defer f.Close()

	reader := bufio.NewReader(f)
	for {
		line, rerr := reader.ReadString('\n')
		if len(line) > 0 {
			onLine(strings.TrimRight(line, "\n"))
			continue
		}
		if rerr != io.EOF && rerr != nil {
			return path, rerr
		}
		// At EOF: wait briefly for more output, then retry. Cancelling stops cleanly.
		select {
		case <-ctx.Done():
			return path, nil
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// killSpace tears down a bare space via the same Registry as `sklp space down`,
// volumes included. Keeping them back left the space dir on disk, so the space
// reappeared on the next refresh and the delete key looked broken.
func killSpace(name string) error {
	reg, err := newSpaceRegistry()
	if err != nil {
		return err
	}
	return reg.Down(context.Background(), name)
}

// stopSpace halts a space but keeps its named volumes, the `space stop` path.
func stopSpace(name string) error {
	reg, err := newSpaceRegistry()
	if err != nil {
		return err
	}
	return reg.Stop(context.Background(), name)
}

// purgeSpaces tears down every space with no running container, the same path
// `sklp clean --deep` takes. The registry decides liveness, so a running
// deployment is kept even if the cockpit's last refresh showed it idle.
func purgeSpaces() ([]string, error) {
	reg, err := newSpaceRegistry()
	if err != nil {
		return nil, err
	}
	return reg.Purge(context.Background())
}

// streamLogs follows svc's logs in space via the control API, calling onLine per
// line until ctx is cancelled. A cancelled follow returns nil (clean stop).
func streamLogs(ctx context.Context, node, space, svc string, onLine func(string)) error {
	client, err := logClient(node, space)
	if err != nil {
		return err
	}
	w := &lineWriter{onLine: onLine}
	if err := client.Logs(ctx, svc, true, w); err != nil {
		return err
	}
	w.flush()
	return nil
}

// lineWriter splits the stream on newlines, invoking onLine per complete line.
type lineWriter struct {
	onLine func(string)
	buf    bytes.Buffer
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	for {
		line, err := w.buf.ReadString('\n')
		if err != nil {
			w.buf.Reset()
			w.buf.WriteString(line) // partial remainder
			break
		}
		w.onLine(line[:len(line)-1])
	}
	return len(p), nil
}

// flush emits any trailing partial line (no terminating newline).
func (w *lineWriter) flush() {
	if rest := w.buf.String(); rest != "" {
		w.onLine(rest)
	}
}

// listSpaces returns the FULL space inventory (same source as `sklp space ls`):
// runtime.NewRegistry(control.ListSpaces).List, mapped onto cockpit.SpaceSummary
// so cockpit stays decoupled from the embedded runtime.
func listSpaces() []cockpit.SpaceSummary {
	reg, err := newSpaceRegistry()
	if err != nil {
		return nil
	}
	infos, err := reg.List(context.Background())
	if err != nil {
		return nil
	}
	out := make([]cockpit.SpaceSummary, 0, len(infos))
	for _, info := range infos {
		runc := make([]cockpit.ContainerSummary, 0, len(info.Containers))
		for _, c := range info.Containers {
			runc = append(runc, cockpit.ContainerSummary{ID: c.ID, Status: c.Status})
		}
		out = append(out, cockpit.SpaceSummary{
			Name:       info.Name,
			Status:     info.Status,
			Runc:       runc,
			Volumes:    info.Volumes,
			Supervised: info.Supervised,
			Repo:       info.Repo,
		})
	}
	return out
}

// execIntoContainer opens an interactive shell in one container of a space. It is
// the cockpit's SpaceExec seam and runs only while the TUI has released the
// terminal (tea.Exec), so it can claim the real stdio and put it in raw mode for
// the container PTY.
func execIntoContainer(node, space, container string) error {
	if node != "" {
		return execIntoRemoteContainer(node, space, container)
	}
	reg, err := newSpaceRegistry()
	if err != nil {
		return err
	}
	opts := spaceapi.ExecOpts{TTY: true, Container: container}
	eio := spaceapi.ExecIO{Stdin: os.Stdin, Stdout: os.Stdout}
	// Name the target before handing the screen over. The cockpit is gone by the
	// time the shell appears, and every container of a space shares one image and
	// one /workspace, so nothing on screen would otherwise say which one this is.
	fmt.Printf("── shell in %s (%s) — exit or Ctrl-D to return to the cockpit ──\n", container, space)
	fd := int(os.Stdin.Fd())
	if st, terr := term.MakeRaw(fd); terr == nil {
		defer func() { _ = term.Restore(fd, st) }()
	}
	if w, h, serr := term.GetSize(int(os.Stdout.Fd())); serr == nil {
		eio.Cols, eio.Rows = uint16(w), uint16(h)
	}
	_, err = reg.Exec(context.Background(), space, opts, eio)
	return err
}

// execIntoRemoteContainer is the node-hosted half of the SpaceExec seam: it
// opens the shell through the node's control API instead of the embedded
// runtime. container names a SERVICE here — the supervisor on the far side
// resolves it to a container of its own.
func execIntoRemoteContainer(node, space, container string) error {
	client, err := logClient(node, space)
	if err != nil {
		return err
	}
	fmt.Printf("── shell in %s (%s on %s) — exit or Ctrl-D to return to the cockpit ──\n", container, space, node)
	tty, rows, cols, restore := setupRemoteExecTerminal()
	if restore != nil {
		defer restore()
	}
	code, err := client.Exec(context.Background(), container, []string{"sh"}, tty, rows, cols, os.Stdin, os.Stdout, os.Stderr)
	if restore != nil {
		restore()
	}
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("shell exited with status %d", code)
	}
	return nil
}

// readFlow derives the flow lifecycle snapshot from the read-only flow.Inspect
// diff + git worktree. CI/PR are best-effort: with no cheap local signal they
// stay Unknown rather than being fabricated.
func readFlow() cockpit.FlowState {
	res, err := flow.Inspect(cockpit.Onto)
	if err != nil {
		// Not a feature branch / no repo: collapsed banner, no error noise.
		return cockpit.FlowState{Onto: cockpit.Onto, CI: cockpit.MilestoneUnknown, PR: cockpit.MilestoneUnknown}
	}
	f := cockpit.FlowState{
		Branch:   res.Branch,
		Onto:     res.Onto,
		Worktree: flow.Worktree(),
		Changed:  len(res.Changed),
		CI:       cockpit.MilestoneUnknown,
		PR:       cockpit.MilestoneUnknown,
	}
	if f.Branch != "" && f.Branch != res.Onto {
		f.Start = cockpit.MilestoneDone
	} else {
		f.Start = cockpit.MilestoneTodo
	}
	if f.Changed > 0 {
		f.Coding = cockpit.MilestoneActive
	} else {
		f.Coding = cockpit.MilestoneTodo
	}
	return f
}

func cockpitCommand() cli.Command {
	return cli.Command{
		Name:  "cockpit",
		Short: "Full-screen read-first monitor: flow lifecycle + live spaces/services",
		Run: func(ctx context.Context, args cli.Args) int {
			return runCockpit(ctx)
		},
	}
}

func runCockpit(ctx context.Context) int {
	// Non-TTY (piped/CI): no one can answer a picker, so report the local
	// machine as before — one-shot snapshot via the plain Renderer, same view.
	if !term.IsTerminal(int(os.Stdout.Fd())) {
		fmt.Println(cockpit.Render(cockpit.Load(ctx, cockpit.LocalContext), cockpit.NewPlainRenderer()))
		return 0
	}

	// Interactive: open on the context picker and read nothing until a node is
	// chosen, so an unrelated powered-off VM is never dialled.
	m := &cockpit.Model{}
	m.OpenContexts()

	app := cockpit.NewBubbleApp()
	if err := app.Run(ctx, m, cockpit.NewLipglossRenderer()); err != nil {
		fmt.Fprintln(os.Stderr, "sklp cockpit:", err)
		return 1
	}
	return 0
}
