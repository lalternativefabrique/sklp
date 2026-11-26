package sklpmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lalternativefabrique/sklp/internal/layout"
)

const (
	quickTimeout      = 2 * time.Minute
	defaultExecWait   = 60
	maxExecWait       = 600
	maxOutputLines    = 500
	defaultJobLines   = 50
	maxStdoutBytes    = 4 << 20
	errorContextLines = 20
)

// local runs this machine's sklp verbs as child processes of the very binary
// serving MCP: on macOS that binary already relays them into the Linux VM, and
// none of their output can reach the protocol's stdout.
type local struct {
	exe  string
	jobs *jobs
}

func childEnv() []string {
	return append(os.Environ(), "NO_COLOR=1", "TERM=dumb")
}

type cmdResult struct {
	stdout   []byte
	output   []string
	dropped  bool
	exitCode int
}

func (l *local) run(ctx context.Context, dir string, timeout time.Duration, args ...string) (cmdResult, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, l.exe, args...)
	cmd.Dir = dir
	cmd.Env = childEnv()
	cmd.Stdin = nil
	var stdout limitedBuffer
	stdout.max = maxStdoutBytes
	ring := &lineRing{max: maxOutputLines}
	cmd.Stdout = io.MultiWriter(&stdout, ring)
	cmd.Stderr = ring
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 10 * time.Second
	err := cmd.Run()
	out, dropped := ring.snapshot()
	res := cmdResult{stdout: stdout.Bytes(), output: nonNil(out), dropped: dropped}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case ctx.Err() != nil:
		return res, fmt.Errorf("sklp %s did not finish within %s", strings.Join(args, " "), timeout)
	case errors.As(err, &exitErr):
		res.exitCode = exitErr.ExitCode()
	default:
		return res, err
	}
	return res, nil
}

// runOK is run for a verb whose failure is the tool's failure.
func (l *local) runOK(ctx context.Context, dir string, args ...string) (cmdResult, error) {
	res, err := l.run(ctx, dir, quickTimeout, args...)
	if err != nil {
		return res, err
	}
	if res.exitCode != 0 {
		return res, fmt.Errorf("sklp %s exited %d: %s", strings.Join(args, " "), res.exitCode, strings.Join(tail(res.output, errorContextLines), "\n"))
	}
	return res, nil
}

type limitedBuffer struct {
	bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); room > 0 {
		b.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (l *local) tools() []toolDef {
	return []toolDef{
		define(&mcp.Tool{Name: "stacks", Description: "List what a repository declares under .sklp/: each stack with its contracts " +
			"(dev, deploy, ...) and the pipelines (ci, publish, ...). Start here."}, read, l.stacks),

		define(&mcp.Tool{Name: "dev_validate", Description: "Parse and validate a stack's dev.yaml without launching it (sklp dev <stack> -validate -check)."}, read, l.devValidate),
		define(&mcp.Tool{Name: "dev_up", Description: "Start a stack's local dev environment in the background (sklp dev <stack> -d). " +
			"Returns a job id: the job ends once the stack is ready or failed to start; poll job_status. Only one dev stack may run on the machine."}, write, l.devUp),
		define(&mcp.Tool{Name: "dev_ls", Description: "List the dev stacks recorded on this machine, running or not, with their repo and run-log."}, read, l.devLs),
		define(&mcp.Tool{Name: "dev_logs", Description: "Return the last lines of a dev stack's latest run-log (every service's output, prefixed by its name)."}, read, l.devLogs),
		define(&mcp.Tool{Name: "dev_down", Description: "Stop a running dev stack (sklp dev down <stack>)."}, write, l.devDown),

		define(&mcp.Tool{Name: "space_ls", Description: "List the spaces (closed environments: dev, ci, local deploys) on this machine, running or stopped."}, read, l.spaceLs),
		define(&mcp.Tool{Name: "space_get", Description: "Show one local space: status, containers, volumes, repo."}, read, l.spaceGet),
		define(&mcp.Tool{Name: "space_exec", Description: "Run a command in a live local space, without a terminal or stdin (sklp space exec <name> [-c <container>] -- <cmd>). " +
			"Returns its exit code and the last lines of its output."}, destructive, l.spaceExec),
		define(&mcp.Tool{Name: "space_stop", Description: "Stop a local space, keeping its named volumes."}, write, l.spaceStop),
		define(&mcp.Tool{Name: "space_down", Description: "Stop and remove a local space and its named volumes: the data in them is destroyed."}, destructive, l.spaceDown),

		define(&mcp.Tool{Name: "run_pipeline", Description: "Run a pipeline of a repository locally, e.g. ci (sklp run <pipeline>): only the steps whose scope the diff touches. " +
			"Returns a job id; poll job_status."}, write, l.runPipeline),
		define(&mcp.Tool{Name: "job_status", Description: "Show a background job (dev_up, run_pipeline): status, exit code and the last lines of its output. " +
			"Without an id, lists the jobs of this server, newest first."}, read, l.jobStatus),
		define(&mcp.Tool{Name: "job_cancel", Description: "Stop a running job the way Ctrl-C would, so sklp tears down what it started."}, write, l.jobCancel),
	}
}

type repoInput struct {
	Repo string `json:"repo" jsonschema:"path of the repository (relative paths resolve against the server's working directory)"`
}

func repoDir(repo string) (string, error) {
	if repo == "" {
		return "", errors.New("repo is required")
	}
	abs, err := filepath.Abs(repo)
	if err != nil {
		return "", err
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("repo %s is not a directory", abs)
	}
	return abs, nil
}

type stackRow struct {
	Name      string   `json:"name"`
	Contracts []string `json:"contracts"`
}

type stacksOut struct {
	Repo      string     `json:"repo"`
	Stacks    []stackRow `json:"stacks"`
	Pipelines []string   `json:"pipelines"`
}

func (l *local) stacks(_ context.Context, in repoInput, _ *redactor) (stacksOut, error) {
	repo, err := repoDir(in.Repo)
	if err != nil {
		return stacksOut{}, err
	}
	lay := layout.New(repo)
	names, err := lay.Stacks()
	if err != nil {
		return stacksOut{}, err
	}
	out := stacksOut{Repo: repo, Stacks: []stackRow{}, Pipelines: []string{}}
	for _, name := range head(names, maxServices) {
		verbs, _ := lay.Verbs(name)
		out.Stacks = append(out.Stacks, stackRow{Name: name, Contracts: append([]string{}, verbs...)})
	}
	if p, err := lay.Pipelines(); err == nil {
		out.Pipelines = append(out.Pipelines, head(p, maxServices)...)
	}
	return out, nil
}

type stackInput struct {
	Repo  string `json:"repo" jsonschema:"path of the repository"`
	Stack string `json:"stack" jsonschema:"stack name (see stacks)"`
}

type outputOut struct {
	OK        bool     `json:"ok"`
	ExitCode  int      `json:"exit_code"`
	Output    []string `json:"output"`
	Truncated bool     `json:"truncated,omitempty"`
}

func (l *local) devValidate(ctx context.Context, in stackInput, _ *redactor) (outputOut, error) {
	repo, err := repoDir(in.Repo)
	if err != nil {
		return outputOut{}, err
	}
	if in.Stack == "" {
		return outputOut{}, errors.New("stack is required (see stacks)")
	}
	res, err := l.run(ctx, repo, quickTimeout, "dev", in.Stack, "-validate", "-check", "-no-color", "-repo", repo)
	if err != nil {
		return outputOut{}, err
	}
	return outputOut{OK: res.exitCode == 0, ExitCode: res.exitCode, Output: res.output, Truncated: res.dropped}, nil
}

type devUpInput struct {
	Repo  string   `json:"repo" jsonschema:"path of the repository"`
	Stack string   `json:"stack" jsonschema:"stack whose .sklp/<stack>/dev.yaml to run"`
	Only  []string `json:"only,omitempty" jsonschema:"launch only these services (plus their dependencies)"`
}

type devStack struct {
	Repo          string `json:"repo"`
	Stack         string `json:"stack"`
	Running       bool   `json:"running"`
	SupervisorPID int    `json:"supervisor_pid,omitempty"`
	Space         string `json:"space,omitempty"`
	Log           string `json:"log,omitempty"`
}

func (l *local) devStacks(ctx context.Context) ([]devStack, error) {
	res, err := l.runOK(ctx, "", "dev", "ls", "-json")
	if err != nil {
		return nil, err
	}
	var out []devStack
	if err := json.Unmarshal(res.stdout, &out); err != nil {
		return nil, fmt.Errorf("decode sklp dev ls: %w", err)
	}
	return out, nil
}

func (l *local) devUp(ctx context.Context, in devUpInput, _ *redactor) (jobOut, error) {
	repo, err := repoDir(in.Repo)
	if err != nil {
		return jobOut{}, err
	}
	if in.Stack == "" {
		return jobOut{}, errors.New("stack is required (see stacks)")
	}
	running, err := l.devStacks(ctx)
	if err != nil {
		return jobOut{}, err
	}
	for _, s := range running {
		if s.Running {
			return jobOut{}, fmt.Errorf("dev stack %q of %s is already running: one dev stack at a time (dev_down it first)", s.Stack, s.Repo)
		}
	}
	args := []string{"dev", in.Stack, "-d", "-no-color", "-repo", repo}
	if len(in.Only) > 0 {
		args = append(args, "-only", strings.Join(in.Only, ","))
	}
	j, err := l.jobs.start(l.exe, repo, args...)
	if err != nil {
		return jobOut{}, err
	}
	return j.view(0), nil
}

type devLsOut struct {
	Stacks    []devStack `json:"stacks"`
	Truncated bool       `json:"truncated,omitempty"`
}

func (l *local) devLs(ctx context.Context, _ noInput, _ *redactor) (devLsOut, error) {
	stacks, err := l.devStacks(ctx)
	if err != nil {
		return devLsOut{}, err
	}
	out := devLsOut{Stacks: head(stacks, maxSpaces), Truncated: len(stacks) > maxSpaces}
	if out.Stacks == nil {
		out.Stacks = []devStack{}
	}
	return out, nil
}

type devLogsInput struct {
	Repo  string `json:"repo" jsonschema:"path of the repository"`
	Stack string `json:"stack" jsonschema:"stack name"`
	Lines int    `json:"lines,omitempty" jsonschema:"how many of the last lines to return (default 100, at most 500)"`
}

type linesOut struct {
	Lines []string `json:"lines"`
}

func (l *local) devLogs(ctx context.Context, in devLogsInput, r *redactor) (linesOut, error) {
	repo, err := repoDir(in.Repo)
	if err != nil {
		return linesOut{}, err
	}
	if in.Stack == "" {
		return linesOut{}, errors.New("stack is required")
	}
	n := in.Lines
	if n <= 0 {
		n = defaultLogLines
	}
	n = min(n, maxOutputLines)
	res, err := l.runOK(ctx, repo, "dev", "logs", in.Stack, "-repo", repo, "-n", fmt.Sprint(n))
	if err != nil {
		return linesOut{}, err
	}
	lines := strings.Split(strings.TrimRight(string(res.stdout), "\n"), "\n")
	return linesOut{Lines: truncateLines(tail(lines, n))}, nil
}

func truncateLines(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		if len(l) > maxLineBytes {
			l = l[:maxLineBytes]
		}
		out[i] = l
	}
	return out
}

func (l *local) devDown(ctx context.Context, in stackInput, _ *redactor) (outputOut, error) {
	repo, err := repoDir(in.Repo)
	if err != nil {
		return outputOut{}, err
	}
	if in.Stack == "" {
		return outputOut{}, errors.New("stack is required")
	}
	res, err := l.runOK(ctx, repo, "dev", "down", in.Stack, "-repo", repo)
	if err != nil {
		return outputOut{}, err
	}
	return outputOut{OK: true, Output: res.output}, nil
}

type localSpace struct {
	Name       string `json:"name"`
	Status     string `json:"status,omitempty"`
	Supervised bool   `json:"supervised"`
	Containers []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"containers"`
	Volumes []string `json:"volumes,omitempty"`
	Repo    string   `json:"repo,omitempty"`
}

type localSpacesOut struct {
	Spaces    []localSpace `json:"spaces"`
	Truncated bool         `json:"truncated,omitempty"`
}

func (l *local) spaceLs(ctx context.Context, _ noInput, _ *redactor) (localSpacesOut, error) {
	res, err := l.runOK(ctx, "", "space", "ls", "-json")
	if err != nil {
		return localSpacesOut{}, err
	}
	var spaces []localSpace
	if err := json.Unmarshal(res.stdout, &spaces); err != nil {
		return localSpacesOut{}, fmt.Errorf("decode sklp space ls: %w", err)
	}
	out := localSpacesOut{Spaces: head(spaces, maxSpaces), Truncated: len(spaces) > maxSpaces}
	if out.Spaces == nil {
		out.Spaces = []localSpace{}
	}
	return out, nil
}

type nameInput struct {
	Name string `json:"name" jsonschema:"space name (see space_ls)"`
}

func spaceName(name string) error {
	if name == "" || strings.HasPrefix(name, "-") {
		return errors.New("a space name is required (see space_ls)")
	}
	return nil
}

func (l *local) spaceGet(ctx context.Context, in nameInput, _ *redactor) (localSpace, error) {
	if err := spaceName(in.Name); err != nil {
		return localSpace{}, err
	}
	res, err := l.runOK(ctx, "", "space", "get", in.Name, "-json")
	if err != nil {
		return localSpace{}, err
	}
	var s localSpace
	if err := json.Unmarshal(res.stdout, &s); err != nil {
		return localSpace{}, fmt.Errorf("decode sklp space get: %w", err)
	}
	return s, nil
}

type spaceExecInput struct {
	Name           string   `json:"name" jsonschema:"space name (see space_ls)"`
	Container      string   `json:"container,omitempty" jsonschema:"service container to enter in a multi-service space"`
	Command        []string `json:"command" jsonschema:"argv to run, e.g. [\"go\",\"test\",\"./...\"]"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty" jsonschema:"stop waiting after this many seconds (default 60, at most 600)"`
}

func (l *local) spaceExec(ctx context.Context, in spaceExecInput, _ *redactor) (outputOut, error) {
	if err := spaceName(in.Name); err != nil {
		return outputOut{}, err
	}
	if len(in.Command) == 0 {
		return outputOut{}, errors.New("command is required")
	}
	args := []string{"space", "exec", in.Name}
	if in.Container != "" {
		args = append(args, "-c", in.Container)
	}
	args = append(append(args, "--"), in.Command...)
	wait := in.TimeoutSeconds
	if wait <= 0 {
		wait = defaultExecWait
	}
	res, err := l.run(ctx, "", time.Duration(min(wait, maxExecWait))*time.Second, args...)
	if err != nil {
		return outputOut{}, err
	}
	return outputOut{OK: res.exitCode == 0, ExitCode: res.exitCode, Output: truncateLines(res.output), Truncated: res.dropped}, nil
}

func (l *local) spaceStop(ctx context.Context, in nameInput, _ *redactor) (outputOut, error) {
	return l.spaceVerb(ctx, "stop", in.Name)
}

func (l *local) spaceDown(ctx context.Context, in nameInput, _ *redactor) (outputOut, error) {
	return l.spaceVerb(ctx, "down", in.Name)
}

func (l *local) spaceVerb(ctx context.Context, verb, name string) (outputOut, error) {
	if err := spaceName(name); err != nil {
		return outputOut{}, err
	}
	res, err := l.runOK(ctx, "", "space", verb, name)
	if err != nil {
		return outputOut{}, err
	}
	return outputOut{OK: true, Output: res.output}, nil
}

type pipelineInput struct {
	Repo     string `json:"repo" jsonschema:"path of the repository"`
	Pipeline string `json:"pipeline" jsonschema:"pipeline name, the <name> of .sklp/pipelines/<name>.yaml (see stacks)"`
}

func (l *local) runPipeline(_ context.Context, in pipelineInput, _ *redactor) (jobOut, error) {
	repo, err := repoDir(in.Repo)
	if err != nil {
		return jobOut{}, err
	}
	pipelines, err := layout.New(repo).Pipelines()
	if err != nil {
		return jobOut{}, err
	}
	if !slices.Contains(pipelines, in.Pipeline) {
		return jobOut{}, fmt.Errorf("no pipeline %q in %s (have: %s)", in.Pipeline, repo, strings.Join(pipelines, ", "))
	}
	j, err := l.jobs.start(l.exe, repo, "run", in.Pipeline)
	if err != nil {
		return jobOut{}, err
	}
	return j.view(0), nil
}

type jobStatusInput struct {
	ID    string `json:"id,omitempty" jsonschema:"job id returned by dev_up or run_pipeline; empty lists every job"`
	Lines int    `json:"lines,omitempty" jsonschema:"how many of the last output lines to return (default 50, at most 500)"`
}

type jobsOut struct {
	Jobs []jobOut `json:"jobs"`
}

func (l *local) jobStatus(_ context.Context, in jobStatusInput, _ *redactor) (jobsOut, error) {
	if in.ID == "" {
		out := jobsOut{Jobs: []jobOut{}}
		for _, j := range l.jobs.all() {
			out.Jobs = append(out.Jobs, j.view(0))
		}
		return out, nil
	}
	j, err := l.jobs.get(in.ID)
	if err != nil {
		return jobsOut{}, err
	}
	n := in.Lines
	if n <= 0 {
		n = defaultJobLines
	}
	v := j.view(min(n, maxOutputLines))
	v.Output = truncateLines(v.Output)
	return jobsOut{Jobs: []jobOut{v}}, nil
}

type jobIDInput struct {
	ID string `json:"id" jsonschema:"job id"`
}

func (l *local) jobCancel(ctx context.Context, in jobIDInput, _ *redactor) (jobOut, error) {
	j, err := l.jobs.get(in.ID)
	if err != nil {
		return jobOut{}, err
	}
	j.stop(ctx)
	return j.view(defaultJobLines), nil
}
