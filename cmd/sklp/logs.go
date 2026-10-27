// logs.go implements `sklp logs` — query stack-aggregated telemetry logs.
//
// The point of this command is that a coding agent can read production logs
// without ever seeing a credential: the token stays in the local config, the
// stack is derived from the project, and time is relative (--since 15m) rather
// than an absolute RFC3339 window the caller must compute.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/lalternativefabrique/sklp/internal/cli"
)

// defaultSince bounds an unqualified invocation. Without it a caller would pull
// the whole retention window on the first call and saturate its context.
const defaultSince = time.Hour

// maxMessageWidth truncates the message column in compact mode. Attributes are
// dropped entirely there — `--json` is the way to get the full record.
const maxMessageWidth = 120

// stackLogItem mirrors telemetry.StackLogResponse. Only the rendered fields are
// mapped; the DTO carries more (trace/span ids, resource, attributes).
type stackLogItem struct {
	Timestamp   time.Time `json:"timestamp"`
	Severity    string    `json:"severity"`
	Service     string    `json:"service"`
	Body        string    `json:"body"`
	ProjectID   string    `json:"project_id"`
	ProjectName string    `json:"project_name"`
}

// stackLogPage mirrors telemetry.StackLogPageResponse.
type stackLogPage struct {
	Items      []stackLogItem `json:"items"`
	NextCursor string         `json:"next_cursor,omitempty"`
	HasMore    bool           `json:"has_more"`
}

// projectStack is the sliver of ProjectResponse used to derive the stack id.
type projectStack struct {
	StackID string `json:"stack_id"`
}

// logsIO bundles stdout/stderr for injection in tests. Color is resolved once
// by the caller rather than probed inside the renderer: the renderer writes to
// an io.Writer, which carries no way to ask "are you a terminal?".
type logsIO struct {
	Stdout io.Writer
	Stderr io.Writer
	Color  bool
}

func defaultLogsIO() *logsIO {
	return &logsIO{Stdout: os.Stdout, Stderr: os.Stderr}
}

// ANSI attributes. Severity is the field the eye scans in a wall of logs, so it
// carries the strongest signal; timestamp and origin are dimmed to stay present
// without competing; the message keeps the default foreground — it IS the
// information.
const (
	ansiReset  = "\033[0m"
	ansiDim    = "\033[2m"
	ansiRed    = "\033[31m"
	ansiYellow = "\033[33m"
	ansiCyan   = "\033[36m"
)

// wantColor decides whether to emit ANSI sequences, following the same policy as
// pipeline/reporter: an explicit --no-color wins, then $NO_COLOR (no-color.org),
// then the output must actually be a terminal. The TTY check is what keeps a
// pipe into jq — or an AI agent reading stdout — free of escape codes.
func wantColor(noColorFlag bool, f *os.File) bool {
	if noColorFlag || os.Getenv("NO_COLOR") != "" {
		return false
	}
	return isTerminalFile(f)
}

// isTerminalFile reports whether f is a character device, the universal TTY
// signature. Same approach as pipeline/reporter's isTerminal and devstack's
// local check — kept package-local rather than exported, matching how those two
// already duplicate it instead of coupling packages over three lines.
func isTerminalFile(f *os.File) bool {
	if f == nil {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// severityColor maps a severity to its attribute. Unknown severities stay
// uncoloured rather than guessing.
func severityColor(sev string) string {
	switch strings.ToUpper(sev) {
	case "ERROR", "FATAL", "CRITICAL":
		return ansiRed
	case "WARN", "WARNING":
		return ansiYellow
	case "DEBUG", "TRACE":
		return ansiDim
	default:
		return ""
	}
}

// paint wraps s in attr, or returns it untouched when colour is off or the
// attribute is empty.
func paint(on bool, attr, s string) string {
	if !on || attr == "" {
		return s
	}
	return attr + s + ansiReset
}

// logsCommand declares `sklp logs` on the façade.
//
// There is deliberately no SKLP_STACK env var: SKLP_PROJECT stays the single
// variable a user or agent has to know, matching `sklp issue`, and the stack is
// derived from it. As with `sklp issue`, deploy profiles must not be able to
// retarget this command — it talks to the Skalpai SaaS only.
func logsCommand() cli.Command {
	return cli.Command{
		Name:  "logs",
		Short: "Query stack logs (severity, time range, full-text)",
		Long: "Reads logs aggregated across a stack's projects.\n" +
			"The stack is derived from --project (or SKLP_PROJECT) unless --stack is given.\n\n" +
			"Examples:\n" +
			"  sklp logs --severity ERROR --since 1h\n" +
			"  sklp logs -q timeout --since 15m --json",
		Flags: []cli.Flag{
			{Name: "project", Type: cli.StringFlag, EnvFallback: "SKLP_PROJECT", Usage: "comma-separated project ids (defaults to SKLP_PROJECT)"},
			{Name: "stack", Type: cli.StringFlag, Usage: "stack id (skips derivation from the project)"},
			{Name: "since", Type: cli.StringFlag, Usage: "relative window, e.g. 15m, 2h (default 1h)"},
			{Name: "from", Type: cli.StringFlag, Usage: "absolute RFC3339 start (mutually exclusive with --since)"},
			{Name: "to", Type: cli.StringFlag, Usage: "absolute RFC3339 end"},
			{Name: "severity", Type: cli.StringFlag, Usage: "filter by severity, e.g. ERROR"},
			{Name: "q", Type: cli.StringFlag, Usage: "search in message and attribute values"},
			{Name: "source", Type: cli.StringFlag, Usage: "filter by origin: browser or server"},
			{Name: "limit", Type: cli.IntFlag, IntDefault: 100, Usage: "page size (max 500)"},
			{Name: "cursor", Type: cli.StringFlag, Usage: "opaque cursor from a previous page"},
			{Name: "json", Type: cli.BoolFlag, Usage: "emit the raw API payload"},
			{Name: "no-color", Type: cli.BoolFlag, Usage: "disable ANSI colour (also honours $NO_COLOR; auto-off when piped)"},
		},
		Run: func(ctx context.Context, args cli.Args) int {
			lio := defaultLogsIO()
			lio.Color = wantColor(args.Bool("no-color"), os.Stdout)
			if err := runLogs(ctx, lio, args); err != nil {
				fmt.Fprintln(os.Stderr, "sklp logs:", err)
				return 1
			}
			return 0
		},
	}
}

func runLogs(ctx context.Context, lio *logsIO, args cli.Args) error {
	q, err := buildLogsQuery(args, time.Now())
	if err != nil {
		return err
	}

	client, err := resolveClient()
	if err != nil {
		return err
	}

	stackID := args.String("stack")
	projects := splitProjectIDs(args.String("project"))
	if stackID == "" {
		if len(projects) == 0 {
			return errors.New("specify --project or --stack, or set SKLP_PROJECT")
		}
		// Derive the stack from the first project rather than introducing a
		// second env var. Ownership is authorised server-side on the stack.
		body, status, err := client.Get(ctx, "/api/projects/"+url.PathEscape(projects[0]))
		if err != nil {
			return wrapAPIError(err, status)
		}
		var p projectStack
		if err := json.Unmarshal(body, &p); err != nil {
			return fmt.Errorf("parse project: %w", err)
		}
		if p.StackID == "" {
			return fmt.Errorf("project %s belongs to no stack; pass --stack", projects[0])
		}
		stackID = p.StackID
	}
	if len(projects) > 0 {
		q.Set("project_ids", strings.Join(projects, ","))
	}

	path := "/api/stacks/" + url.PathEscape(stackID) + "/logs?" + q.Encode()
	body, status, err := client.Get(ctx, path)
	if err != nil {
		return wrapAPIError(err, status)
	}

	if args.Bool("json") {
		if _, werr := lio.Stdout.Write(body); werr != nil {
			return werr
		}
		if len(body) > 0 && body[len(body)-1] != '\n' {
			fmt.Fprintln(lio.Stdout)
		}
		return nil
	}

	var page stackLogPage
	if err := json.Unmarshal(body, &page); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	return renderLogs(lio, page)
}

// buildLogsQuery turns flags into API query params. now is injected so the
// --since conversion is testable.
func buildLogsQuery(args cli.Args, now time.Time) (url.Values, error) {
	since, from := args.String("since"), args.String("from")
	if since != "" && from != "" {
		return nil, errors.New("--since and --from are mutually exclusive")
	}

	q := url.Values{}
	switch {
	case from != "":
		if _, err := time.Parse(time.RFC3339, from); err != nil {
			return nil, fmt.Errorf("invalid --from (want RFC3339): %w", err)
		}
		q.Set("from", from)
	default:
		d := defaultSince
		if since != "" {
			parsed, err := time.ParseDuration(since)
			if err != nil {
				return nil, fmt.Errorf("invalid --since (want e.g. 15m, 2h): %w", err)
			}
			if parsed <= 0 {
				return nil, errors.New("--since must be positive")
			}
			d = parsed
		}
		q.Set("from", now.Add(-d).UTC().Format(time.RFC3339))
	}

	if to := args.String("to"); to != "" {
		if _, err := time.Parse(time.RFC3339, to); err != nil {
			return nil, fmt.Errorf("invalid --to (want RFC3339): %w", err)
		}
		q.Set("to", to)
	}
	for flag, param := range map[string]string{"severity": "severity", "q": "q", "source": "source", "cursor": "cursor"} {
		if v := args.String(flag); v != "" {
			q.Set(param, v)
		}
	}
	if limit := args.Int("limit"); limit > 0 {
		q.Set("limit", fmt.Sprint(limit))
	}
	return q, nil
}

// splitProjectIDs parses the comma-separated --project value, dropping blanks.
func splitProjectIDs(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// renderLogs writes one compact line per record. Notices go to stderr so stdout
// stays pipeable into jq, and so an empty result is distinguishable from a
// broken query.
func renderLogs(lio *logsIO, page stackLogPage) error {
	if len(page.Items) == 0 {
		fmt.Fprintln(lio.Stderr, "# no logs in range")
		return nil
	}
	for _, it := range page.Items {
		// Pad BEFORE colouring: %-6s counts bytes, so an ANSI-wrapped severity
		// would blow the column width apart.
		sev := fmt.Sprintf("%-6s", strings.ToUpper(it.Severity))
		fmt.Fprintf(lio.Stdout, "%s %s %s %s\n",
			paint(lio.Color, ansiDim, it.Timestamp.UTC().Format(time.RFC3339)),
			paint(lio.Color, severityColor(it.Severity), sev),
			paint(lio.Color, ansiCyan, logOrigin(it)),
			truncateMessage(it.Body),
		)
	}
	if page.HasMore {
		fmt.Fprintf(lio.Stderr,
			"# %d logs shown, more available — narrow --since or pass --cursor %s\n",
			len(page.Items), page.NextCursor)
	}
	return nil
}

// logOrigin renders the "where did this come from" column. Project and service
// are both kept when they differ: a project groups several services, and the
// service is usually the discriminant when triaging.
func logOrigin(it stackLogItem) string {
	switch {
	case it.ProjectName == "":
		return it.Service
	case it.Service == "" || strings.EqualFold(it.Service, it.ProjectName):
		return it.ProjectName
	default:
		return it.ProjectName + "/" + it.Service
	}
}

// truncateMessage collapses newlines and caps width so one record stays one line.
func truncateMessage(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", " "), "\n", " "))
	if len([]rune(s)) > maxMessageWidth {
		return string([]rune(s)[:maxMessageWidth]) + "…"
	}
	return s
}
