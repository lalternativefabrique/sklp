// ci.go implements `sklp ci {status,watch}` — following a remote CI run
// (dispatched via GET/POST /api/projects/:id/ci/runs) from the terminal,
// instead of the Skalpai UI being the only place to see it.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"time"

	"github.com/lalternativefabrique/sklp/cmd/sklp/internal/apiclient"
	"github.com/lalternativefabrique/sklp/internal/cli"
)

// runDTO mirrors ci.RunDTO (the server's ci.RunDTO). Unknown fields tolerated.
type runDTO struct {
	ID          string     `json:"id"`
	ProjectID   string     `json:"project_id"`
	TriggerType string     `json:"trigger_type"`
	HeadRef     string     `json:"head_ref"`
	BaseRef     string     `json:"base_ref,omitempty"`
	PRNumber    int        `json:"pr_number,omitempty"`
	TaskName    string     `json:"task_name"`
	Status      string     `json:"status"`
	PassFail    string     `json:"pass_fail,omitempty"`
	ErrorMsg    string     `json:"error_message,omitempty"`
	Phase       string     `json:"phase,omitempty"`
	Node        string     `json:"node,omitempty"`
	StepsDone   int        `json:"steps_done,omitempty"`
	StepsTotal  int        `json:"steps_total,omitempty"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

func (r runDTO) terminal() bool {
	switch r.Status {
	case "succeeded", "failed", "aborted":
		return true
	default:
		return false
	}
}

func (r runDTO) exitCode() int {
	if r.PassFail == "fail" || r.Status == "failed" || r.Status == "aborted" {
		return 1
	}
	return 0
}

type logLineDTO struct {
	TS     time.Time `json:"ts"`
	Stream string    `json:"stream"`
	Line   string    `json:"line"`
}

type runStepEvent struct {
	RunID      string `json:"run_id"`
	Name       string `json:"name"`
	Phase      string `json:"phase"`
	Status     string `json:"status,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}

type runLogLineEvent struct {
	RunID  string `json:"run_id"`
	Stream string `json:"stream"`
	Line   string `json:"line"`
	Step   int    `json:"step,omitempty"`
}

type runCompletedEvent struct {
	RunID        string  `json:"run_id"`
	Status       string  `json:"status"`
	PassFail     string  `json:"pass_fail,omitempty"`
	ErrorMessage string  `json:"error_message,omitempty"`
	Report       *string `json:"report,omitempty"`
	CompletedAt  string  `json:"completed_at"`
}

func ciCommand() cli.Command {
	run := func(fn func(*issueIO, cli.Args) error) func(context.Context, cli.Args) int {
		return func(ctx context.Context, args cli.Args) int {
			if err := fn(defaultIssueIO(), args); err != nil {
				fmt.Fprintln(os.Stderr, "sklp ci:", err)
				return 1
			}
			return 0
		}
	}
	// watch's exit code must reflect the run's pass/fail, not just transport
	// errors, so it gets its own closure instead of the shared `run`.
	watchRun := func(ctx context.Context, args cli.Args) int {
		code, err := ciWatch(ctx, defaultIssueIO(), args)
		if err != nil {
			fmt.Fprintln(os.Stderr, "sklp ci:", err)
			return 1
		}
		return code
	}
	idPos := []cli.Positional{{Name: "run-id", Required: false}}
	return cli.Command{
		Name:  "ci",
		Short: "Follow a remote CI run (status | watch)",
		Sub: []cli.Command{
			{Name: "status", Short: "Show a run's current status (no tail)", Flags: ciFlags(), Positionals: idPos, Run: run(ciStatus)},
			{Name: "watch", Short: "Replay + tail a run's logs until it completes", Flags: ciFlags(), Positionals: idPos, Run: watchRun},
		},
	}
}

func ciFlags() []cli.Flag {
	return []cli.Flag{
		issueProjectFlag,
		{Name: "branch", Type: cli.StringFlag, Usage: "resolve the latest run for this head_ref instead of passing a run id"},
		issueJSONFlag,
	}
}

// resolveRun picks the run id: the positional if given, else the most
// recent run for --project (optionally filtered by --branch).
func resolveRun(ctx context.Context, c *apiclient.Client, project string, args cli.Args) (project_ string, runID string, err error) {
	if project == "" {
		return "", "", errors.New("specify --project or set SKLP_PROJECT")
	}
	if id := firstArg(args, 0); id != "" {
		return project, id, nil
	}

	path := "/api/projects/" + url.PathEscape(project) + "/ci/runs?limit=1"
	if branch := args.String("branch"); branch != "" {
		path += "&head_ref=" + url.QueryEscape(branch)
	}
	body, status, err := c.Get(ctx, path)
	if err != nil {
		return "", "", wrapAPIError(err, status)
	}
	var listResp struct {
		Runs []runDTO `json:"runs"`
	}
	if err := json.Unmarshal(body, &listResp); err != nil {
		return "", "", fmt.Errorf("parse response: %w", err)
	}
	if len(listResp.Runs) == 0 {
		return "", "", errors.New("no CI runs found for this project")
	}
	return project, listResp.Runs[0].ID, nil
}

func ciStatus(iio *issueIO, args cli.Args) error {
	c, err := resolveClient()
	if err != nil {
		return err
	}
	ctx := context.Background()
	project := pickProject(args.String("project"))
	project, runID, err := resolveRun(ctx, c, project, args)
	if err != nil {
		return err
	}

	body, status, err := c.Get(ctx, "/api/projects/"+url.PathEscape(project)+"/ci/runs/"+url.PathEscape(runID))
	if err != nil {
		return wrapAPIError(err, status)
	}
	if args.Bool("json") {
		_, werr := iio.Stdout.Write(body)
		if werr != nil {
			return werr
		}
		if len(body) > 0 && body[len(body)-1] != '\n' {
			fmt.Fprintln(iio.Stdout)
		}
		return nil
	}

	var run runDTO
	if err := json.Unmarshal(body, &run); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	printRunStatus(iio.Stdout, run)
	return nil
}

func printRunStatus(w io.Writer, run runDTO) {
	fmt.Fprintf(w, "run:     %s\n", run.ID)
	fmt.Fprintf(w, "branch:  %s\n", run.HeadRef)
	if run.PRNumber > 0 {
		fmt.Fprintf(w, "pr:      #%d\n", run.PRNumber)
	}
	status := run.Status
	if run.Phase != "" && !run.terminal() {
		status = fmt.Sprintf("%s (%s)", status, run.Phase)
	}
	fmt.Fprintf(w, "status:  %s\n", status)
	if run.StepsTotal > 0 {
		fmt.Fprintf(w, "steps:   %d/%d\n", run.StepsDone, run.StepsTotal)
	}
	if run.Node != "" {
		fmt.Fprintf(w, "node:    %s\n", run.Node)
	}
	if run.ErrorMsg != "" {
		fmt.Fprintf(w, "error:   %s\n", run.ErrorMsg)
	}
}

// ciWatch replays persisted logs, then tails the SSE stream until a
// terminal frame arrives. Returns the process exit code to use.
func ciWatch(ctx context.Context, iio *issueIO, args cli.Args) (int, error) {
	c, err := resolveClient()
	if err != nil {
		return 1, err
	}
	project := pickProject(args.String("project"))
	project, runID, err := resolveRun(ctx, c, project, args)
	if err != nil {
		return 1, err
	}
	base := "/api/projects/" + url.PathEscape(project) + "/ci/runs/" + url.PathEscape(runID)

	logsBody, status, err := c.Get(ctx, base+"/logs")
	if err != nil {
		return 1, wrapAPIError(err, status)
	}
	var lines []logLineDTO
	if err := json.Unmarshal(logsBody, &lines); err != nil {
		return 1, fmt.Errorf("parse logs: %w", err)
	}
	for _, l := range lines {
		fmt.Fprintln(iio.Stdout, l.Line)
	}

	runBody, status, err := c.Get(ctx, base)
	if err != nil {
		return 1, wrapAPIError(err, status)
	}
	var run runDTO
	if err := json.Unmarshal(runBody, &run); err != nil {
		return 1, fmt.Errorf("parse run: %w", err)
	}
	if run.terminal() {
		printRunStatus(iio.Stderr, run)
		return run.exitCode(), nil
	}

	// The SSE connection does not outlive an intermediate proxy's idle
	// timeout (observed ~30-50s against the remote CI node), and unlike a
	// browser's EventSource this client does not reconnect on its own — a
	// run that legitimately keeps executing past that window used to end
	// the whole `watch` with "stream closed"/"unexpected EOF" instead of
	// continuing to tail it. Reopen the stream on any non-terminal drop.
	for {
		events, errc, err := c.StreamSSE(ctx, base+"/events")
		if err != nil {
			return 1, wrapAPIError(err, status)
		}

		code, done, err := ciWatchDrain(ctx, iio, events, errc)
		if done {
			return code, err
		}
		if err != nil {
			return 1, err
		}

		runBody, status, err := c.Get(ctx, base)
		if err != nil {
			return 1, wrapAPIError(err, status)
		}
		if err := json.Unmarshal(runBody, &run); err != nil {
			return 1, fmt.Errorf("parse run: %w", err)
		}
		if run.terminal() {
			printRunStatus(iio.Stderr, run)
			return run.exitCode(), nil
		}
		// Still running — the drop was the proxy, not the run. Reconnect.
	}
}

// ciWatchDrain consumes one SSE connection's worth of frames. done is true
// once a "completed" event settles the run's outcome (code, err are then
// final) or the context is cancelled; done is false when the stream ends
// or errors before a "completed" frame arrives, meaning the caller should
// check the run's actual status and decide whether to reconnect.
func ciWatchDrain(ctx context.Context, iio *issueIO, events <-chan apiclient.SSEEvent, errc <-chan error) (code int, done bool, err error) {
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return 0, false, nil
			}
			switch ev.Name {
			case "log":
				var le runLogLineEvent
				if json.Unmarshal(ev.Data, &le) == nil {
					fmt.Fprintln(iio.Stdout, le.Line)
				}
			case "step":
				var se runStepEvent
				if json.Unmarshal(ev.Data, &se) == nil {
					fmt.Fprintf(iio.Stderr, "· %s %s%s\n", se.Name, se.Phase, stepStatusSuffix(se))
				}
			case "completed":
				var ce runCompletedEvent
				if err := json.Unmarshal(ev.Data, &ce); err != nil {
					return 1, true, fmt.Errorf("parse completed event: %w", err)
				}
				fmt.Fprintf(iio.Stderr, "status:  %s\n", ce.Status)
				if ce.ErrorMessage != "" {
					fmt.Fprintf(iio.Stderr, "error:   %s\n", ce.ErrorMessage)
				}
				if ce.PassFail == "fail" || ce.Status == "failed" || ce.Status == "aborted" {
					return 1, true, nil
				}
				return 0, true, nil
			}
		case streamErr := <-errc:
			return 0, false, streamErr
		case <-ctx.Done():
			return 1, true, ctx.Err()
		}
	}
}

func stepStatusSuffix(se runStepEvent) string {
	if se.Status == "" {
		return ""
	}
	return " (" + se.Status + ")"
}
