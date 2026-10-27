// issue.go implements `sklp issue {list,new,show,edit,status,promote,gh-*}`.
// Issues are the actionable work unit devs care about — this is the
// terminal-facing entry point for the issue bounded context.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/lalternativefabrique/sklp/cmd/sklp/internal/apiclient"
	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/internal/config"
)

// issueProjectFlag / issueJSONFlag are shared by every issue sub-verb.
// There is deliberately no profile/context selector here: `sklp issue` talks
// to the Skalpai SaaS, of which the config holds exactly one (the "api"
// block). Deploy contexts are a different control plane and must not be able
// to retarget — or break — the API client.
var (
	issueProjectFlag = cli.Flag{Name: "project", Type: cli.StringFlag, EnvFallback: "SKLP_PROJECT", Usage: "project id (defaults to SKLP_PROJECT)"}
	issueJSONFlag    = cli.Flag{Name: "json", Type: cli.BoolFlag, Usage: "emit raw JSON from the API"}
)

// issueItem mirrors the IssueResponse / StackIssueResponse DTOs.
// Unknown fields are tolerated — we only map what we render.
type issueItem struct {
	ID                string     `json:"id"`
	ProjectID         string     `json:"project_id"`
	ProjectName       string     `json:"project_name,omitempty"` // only set by /api/stacks/:id/issues
	Title             string     `json:"title"`
	Body              string     `json:"body"`
	Type              string     `json:"type"`
	Status            string     `json:"status"`
	TrackerProvider   string     `json:"tracker_provider,omitempty"`
	TrackerExternalID string     `json:"tracker_external_id,omitempty"`
	TrackerURL        string     `json:"tracker_url,omitempty"`
	ClosedReason      string     `json:"closed_reason,omitempty"`
	StartedAt         *time.Time `json:"started_at,omitempty"`
	ClosedAt          *time.Time `json:"closed_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// issueIO bundles stdout/stderr for injection in tests.
type issueIO struct {
	Stdout io.Writer
	Stderr io.Writer
}

func defaultIssueIO() *issueIO {
	return &issueIO{Stdout: os.Stdout, Stderr: os.Stderr}
}

// issueCommand declares `sklp issue <subcommand>` on the façade. Each sub-verb
// declares its own flags/positionals; the Run is thin glue mapping a returned
// error to exit code 1.
func issueCommand() cli.Command {
	run := func(fn func(*issueIO, cli.Args) error) func(context.Context, cli.Args) int {
		return func(ctx context.Context, args cli.Args) int {
			if err := fn(defaultIssueIO(), args); err != nil {
				fmt.Fprintln(os.Stderr, "sklp issue:", err)
				return 1
			}
			return 0
		}
	}
	gh := func(suffix string) func(context.Context, cli.Args) int {
		return run(func(iio *issueIO, a cli.Args) error { return issueGhTracker(iio, a, suffix) })
	}
	idPos := []cli.Positional{{Name: "id", Required: true}}
	return cli.Command{
		Name:  "issue",
		Short: "Manage project issues (list | new | show | edit | status | promote | gh-*)",
		Long:  "Valid types: bug, feature, task, other. Valid statuses: backlog, in_progress, done, wontfix.",
		Sub: []cli.Command{
			{Name: "list", Short: "List issues for a project or stack", Flags: issueListFlags(), Run: run(issueList)},
			{Name: "new", Short: "Create a new issue (optionally with attachments)", Flags: issueNewFlags(), Positionals: []cli.Positional{{Name: "title", Required: true}}, Run: run(issueNew)},
			{Name: "show", Short: "Show details of a single issue", Flags: []cli.Flag{issueProjectFlag, issueJSONFlag}, Positionals: idPos, Run: run(issueShow)},
			{Name: "edit", Short: "Update title/body/type of an issue", Flags: issueEditFlags(), Positionals: idPos, Run: run(issueEdit)},
			{Name: "status", Short: "Change an issue's workflow status", Flags: issueStatusFlags(), Positionals: []cli.Positional{{Name: "id", Required: true}, {Name: "status", Required: true}}, Run: run(issueStatus)},
			{Name: "promote", Short: "Promote a feedback into an issue", Flags: issuePromoteFlags(), Positionals: []cli.Positional{{Name: "feedback-id", Required: true}}, Run: run(issuePromote)},
			{Name: "gh-link", Short: "Open a GitHub mirror issue for the issue", Flags: issueGhFlags(), Positionals: idPos, Run: gh("")},
			{Name: "gh-close", Short: "Close the linked GitHub issue (mirror state only)", Flags: issueGhFlags(), Positionals: idPos, Run: gh("/close")},
			{Name: "gh-reopen", Short: "Reopen the linked GitHub issue (mirror state only)", Flags: issueGhFlags(), Positionals: idPos, Run: gh("/reopen")},
			{Name: "gh-unlink", Short: "Drop the GitHub tracker link", Flags: []cli.Flag{issueProjectFlag}, Positionals: idPos, Run: run(issueGhUnlink)},
		},
	}
}

// issue*Flags declare each sub-verb's flag set, reused by issueCommand and the
// parsing tests.
func issueListFlags() []cli.Flag {
	return []cli.Flag{
		issueProjectFlag,
		{Name: "stack", Type: cli.StringFlag, Usage: "stack id (alternative to --project)"},
		{Name: "status", Type: cli.StringFlag, Usage: "filter by status (backlog|in_progress|done|wontfix)"},
		{Name: "type", Type: cli.StringFlag, Usage: "filter by type (bug|feature|task|other) — client-side"},
		{Name: "limit", Type: cli.IntFlag, Usage: "cap the number of rows shown (0 = no cap)"},
		issueJSONFlag,
		{Name: "plain", Type: cli.BoolFlag, Usage: "emit TSV (one row per issue, no header)"},
	}
}

func issueNewFlags() []cli.Flag {
	return []cli.Flag{
		issueProjectFlag,
		{Name: "body", Type: cli.StringFlag, Usage: "issue body (markdown)"},
		{Name: "type", Type: cli.StringFlag, Usage: "type (bug|feature|task|other)"},
		{Name: "status", Type: cli.StringFlag, Usage: "initial status (backlog|in_progress|done|wontfix)"},
		{Name: "attach", Type: cli.StringSliceFlag, Usage: "path to a file to attach (repeatable)"},
		issueJSONFlag,
	}
}

func issueEditFlags() []cli.Flag {
	return []cli.Flag{
		issueProjectFlag,
		{Name: "title", Type: cli.StringFlag, Usage: "new title (omit to keep)"},
		{Name: "body", Type: cli.StringFlag, Usage: "new body (omit to keep; empty string clears)"},
		{Name: "type", Type: cli.StringFlag, Usage: "new type (bug|feature|task|other)"},
		issueJSONFlag,
	}
}

func issueStatusFlags() []cli.Flag {
	return []cli.Flag{
		issueProjectFlag,
		{Name: "reason", Type: cli.StringFlag, Usage: "closed_reason (used when status closes the issue)"},
		issueJSONFlag,
	}
}

func issuePromoteFlags() []cli.Flag {
	return []cli.Flag{
		issueProjectFlag,
		{Name: "title", Type: cli.StringFlag, Usage: "override the promoted issue title"},
		{Name: "body", Type: cli.StringFlag, Usage: "override the promoted issue body"},
		{Name: "type", Type: cli.StringFlag, Usage: "override the promoted issue type"},
		{Name: "status", Type: cli.StringFlag, Usage: "initial status for the new issue"},
		issueJSONFlag,
	}
}

func issueGhFlags() []cli.Flag {
	return []cli.Flag{
		issueProjectFlag,
		{Name: "title", Type: cli.StringFlag, Usage: "title override (only used by gh-link)"},
		{Name: "body", Type: cli.StringFlag, Usage: "body override (only used by gh-link)"},
		issueJSONFlag,
	}
}

// firstArg returns the positional at index i, or "".
func firstArg(a cli.Args, i int) string {
	pos := a.Positionals()
	if i < len(pos) {
		return pos[i]
	}
	return ""
}

// resolveClient returns an authenticated apiclient.Client for the Skalpai
// SaaS. Shared with other CLI verbs.
//
// No selector is involved: ResolveAPI reads the single "api" block, so no
// deploy-context state can make this fail. The old code resolved a profile
// here and, when the active profile was a deploy node, reported "profile has
// no api_url (run sklp auth login)" — a remedy that did not fix anything,
// since the user WAS logged in and had merely switched deploy context.
func resolveClient() (*apiclient.Client, error) {
	cfg, _, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	api, err := cfg.ResolveAPI()
	if err != nil {
		return nil, err
	}
	token := config.EffectiveAPIToken(api)
	if token == "" {
		return nil, errors.New("not logged in (run sklp auth login)")
	}
	return apiclient.New(api.URL, token), nil
}

// pickProject resolves the project id from flag → SKLP_PROJECT env.
func pickProject(flagProject string) string {
	if flagProject != "" {
		return flagProject
	}
	return os.Getenv("SKLP_PROJECT")
}

func issueList(iio *issueIO, args cli.Args) error {
	if args.Bool("json") && args.Bool("plain") {
		return errors.New("--json and --plain are mutually exclusive")
	}

	cli, err := resolveClient()
	if err != nil {
		return err
	}

	project := pickProject(args.String("project"))
	stackFlag := args.String("stack")
	var path string
	switch {
	case project != "":
		path = "/api/projects/" + url.PathEscape(project) + "/issues"
	case stackFlag != "":
		path = "/api/stacks/" + url.PathEscape(stackFlag) + "/issues"
	default:
		return errors.New("specify --project or --stack, or set SKLP_PROJECT")
	}
	// Status filter is pushed server-side when set; type is filtered client-side.
	if s := args.String("status"); s != "" {
		path += "?status=" + url.QueryEscape(s)
	}

	body, status, err := cli.Get(context.Background(), path)
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

	var items []issueItem
	if err := json.Unmarshal(body, &items); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	if t := args.String("type"); t != "" {
		items = filterIssuesByType(items, t)
	}
	if limit := args.Int("limit"); limit > 0 && len(items) > limit {
		items = items[:limit]
	}

	if args.Bool("plain") {
		return renderIssuesPlain(iio.Stdout, items)
	}
	return renderIssuesTable(iio.Stdout, items)
}

func issueNew(iio *issueIO, args cli.Args) error {
	title := strings.TrimSpace(firstArg(args, 0))
	if title == "" {
		return errors.New("title must not be empty")
	}

	project := pickProject(args.String("project"))
	if project == "" {
		return errors.New("specify --project or set SKLP_PROJECT")
	}

	cli, err := resolveClient()
	if err != nil {
		return err
	}

	bodyFlag := args.String("body")
	typeFlag := args.String("type")
	statusFlag := args.String("status")
	attachFlag := args.StringSlice("attach")

	path := "/api/projects/" + url.PathEscape(project) + "/issues"
	var (
		body   []byte
		status int
	)
	if len(attachFlag) > 0 {
		buf, contentType, berr := buildIssueMultipart(title, bodyFlag, typeFlag, statusFlag, attachFlag)
		if berr != nil {
			return berr
		}
		body, status, err = cli.PostRaw(context.Background(), path, contentType, buf)
	} else {
		payload := map[string]string{"title": title}
		if bodyFlag != "" {
			payload["body"] = bodyFlag
		}
		if typeFlag != "" {
			payload["type"] = typeFlag
		}
		if statusFlag != "" {
			payload["status"] = statusFlag
		}
		body, status, err = cli.Post(context.Background(), path, payload)
	}
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

	var resp issueItem
	_ = json.Unmarshal(body, &resp)
	if resp.ID != "" {
		fmt.Fprintf(iio.Stdout, "created issue %s\n", resp.ID)
	} else {
		fmt.Fprintln(iio.Stdout, "created issue")
	}
	return nil
}

// buildIssueMultipart streams the title/body/type/status form fields plus
// every --attach file into a multipart body. Returns the buffer, the
// Content-Type header (with boundary), or an error if any file is unreadable.
func buildIssueMultipart(title, body, typ, status string, files []string) (io.Reader, string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("title", title); err != nil {
		return nil, "", err
	}
	if body != "" {
		if err := mw.WriteField("body", body); err != nil {
			return nil, "", err
		}
	}
	if typ != "" {
		if err := mw.WriteField("type", typ); err != nil {
			return nil, "", err
		}
	}
	if status != "" {
		if err := mw.WriteField("status", status); err != nil {
			return nil, "", err
		}
	}
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			return nil, "", fmt.Errorf("open attachment %s: %w", path, err)
		}
		part, err := mw.CreateFormFile("attachments", filepath.Base(path))
		if err != nil {
			_ = f.Close()
			return nil, "", err
		}
		if _, err := io.Copy(part, f); err != nil {
			_ = f.Close()
			return nil, "", fmt.Errorf("read attachment %s: %w", path, err)
		}
		_ = f.Close()
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return &buf, mw.FormDataContentType(), nil
}

func issueShow(iio *issueIO, args cli.Args) error {
	issueID := firstArg(args, 0)
	if issueID == "" {
		return errors.New("usage: sklp issue show <id> [flags]")
	}

	project := pickProject(args.String("project"))
	if project == "" {
		return errors.New("specify --project or set SKLP_PROJECT")
	}

	cli, err := resolveClient()
	if err != nil {
		return err
	}

	// No single-issue GET endpoint — list + filter, same as feedback show.
	body, status, err := cli.Get(context.Background(),
		"/api/projects/"+url.PathEscape(project)+"/issues")
	if err != nil {
		return wrapAPIError(err, status)
	}

	var items []issueItem
	if err := json.Unmarshal(body, &items); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	var found *issueItem
	for i := range items {
		if items[i].ID == issueID {
			found = &items[i]
			break
		}
	}
	if found == nil {
		return fmt.Errorf("issue %s not found in project %s", issueID, project)
	}

	if args.Bool("json") {
		out, _ := json.MarshalIndent(found, "", "  ")
		fmt.Fprintln(iio.Stdout, string(out))
		return nil
	}

	renderIssueShow(iio.Stdout, found)
	return nil
}

func issueEdit(iio *issueIO, args cli.Args) error {
	issueID := firstArg(args, 0)
	if issueID == "" {
		return errors.New("usage: sklp issue edit <id> [flags]")
	}

	project := pickProject(args.String("project"))
	if project == "" {
		return errors.New("specify --project or set SKLP_PROJECT")
	}

	// Mirror the DTO's pointer semantics: only set fields the caller passed.
	// IsSet distinguishes "omit to keep" from "--body '' clears".
	payload := map[string]any{}
	if args.IsSet("title") {
		payload["title"] = args.String("title")
	}
	if args.IsSet("body") {
		payload["body"] = args.String("body")
	}
	if args.IsSet("type") {
		payload["type"] = args.String("type")
	}
	if len(payload) == 0 {
		return errors.New("specify at least one of --title, --body, --type")
	}

	cli, err := resolveClient()
	if err != nil {
		return err
	}

	body, status, err := cli.Patch(context.Background(),
		"/api/projects/"+url.PathEscape(project)+"/issues/"+url.PathEscape(issueID), payload)
	if err != nil {
		return wrapAPIError(err, status)
	}
	return printIssueResult(iio, body, args.Bool("json"), "updated")
}

func issueStatus(iio *issueIO, args cli.Args) error {
	issueID := firstArg(args, 0)
	newStatus := firstArg(args, 1)
	if issueID == "" || newStatus == "" {
		return errors.New("usage: sklp issue status <id> <status> [flags]")
	}

	project := pickProject(args.String("project"))
	if project == "" {
		return errors.New("specify --project or set SKLP_PROJECT")
	}

	cli, err := resolveClient()
	if err != nil {
		return err
	}

	payload := map[string]string{"status": newStatus}
	if r := args.String("reason"); r != "" {
		payload["closed_reason"] = r
	}
	body, status, err := cli.Post(context.Background(),
		"/api/projects/"+url.PathEscape(project)+"/issues/"+url.PathEscape(issueID)+"/status",
		payload)
	if err != nil {
		return wrapAPIError(err, status)
	}
	return printIssueResult(iio, body, args.Bool("json"), "status updated")
}

func issuePromote(iio *issueIO, args cli.Args) error {
	feedbackID := firstArg(args, 0)
	if feedbackID == "" {
		return errors.New("usage: sklp issue promote <feedback-id> [flags]")
	}

	project := pickProject(args.String("project"))
	if project == "" {
		return errors.New("specify --project or set SKLP_PROJECT")
	}

	cli, err := resolveClient()
	if err != nil {
		return err
	}

	payload := map[string]string{}
	if v := args.String("title"); v != "" {
		payload["title"] = v
	}
	if v := args.String("body"); v != "" {
		payload["body"] = v
	}
	if v := args.String("type"); v != "" {
		payload["type"] = v
	}
	if v := args.String("status"); v != "" {
		payload["status"] = v
	}

	body, status, err := cli.Post(context.Background(),
		"/api/projects/"+url.PathEscape(project)+"/feedback/"+url.PathEscape(feedbackID)+"/promote",
		payload)
	if err != nil {
		return wrapAPIError(err, status)
	}
	return printIssueResult(iio, body, args.Bool("json"), "promoted")
}

// issueGhTracker handles gh-link (suffix "") / gh-close / gh-reopen.
func issueGhTracker(iio *issueIO, args cli.Args, suffix string) error {
	verbName := "gh-link"
	switch suffix {
	case "/close":
		verbName = "gh-close"
	case "/reopen":
		verbName = "gh-reopen"
	}
	issueID := firstArg(args, 0)
	if issueID == "" {
		return fmt.Errorf("usage: sklp issue %s <id> [flags]", verbName)
	}

	project := pickProject(args.String("project"))
	if project == "" {
		return errors.New("specify --project or set SKLP_PROJECT")
	}

	cli, err := resolveClient()
	if err != nil {
		return err
	}

	path := "/api/projects/" + url.PathEscape(project) +
		"/issues/" + url.PathEscape(issueID) + "/tracker/github" + suffix
	payload := map[string]string{}
	if suffix == "" {
		if v := args.String("title"); v != "" {
			payload["title"] = v
		}
		if v := args.String("body"); v != "" {
			payload["body"] = v
		}
	}
	body, status, err := cli.Post(context.Background(), path, payload)
	if err != nil {
		return wrapAPIError(err, status)
	}
	return printIssueResult(iio, body, args.Bool("json"), "tracker "+verbName)
}

func issueGhUnlink(iio *issueIO, args cli.Args) error {
	issueID := firstArg(args, 0)
	if issueID == "" {
		return errors.New("usage: sklp issue gh-unlink <id> [flags]")
	}

	project := pickProject(args.String("project"))
	if project == "" {
		return errors.New("specify --project or set SKLP_PROJECT")
	}

	cli, err := resolveClient()
	if err != nil {
		return err
	}

	_, status, err := cli.Delete(context.Background(),
		"/api/projects/"+url.PathEscape(project)+"/issues/"+url.PathEscape(issueID)+"/tracker")
	if err != nil {
		return wrapAPIError(err, status)
	}
	fmt.Fprintf(iio.Stdout, "tracker unlinked from issue %s\n", issueID)
	return nil
}

// printIssueResult handles the common "POST/PATCH returned an Issue" path.
func printIssueResult(iio *issueIO, body []byte, asJSON bool, label string) error {
	if asJSON {
		_, werr := iio.Stdout.Write(body)
		if werr != nil {
			return werr
		}
		if len(body) > 0 && body[len(body)-1] != '\n' {
			fmt.Fprintln(iio.Stdout)
		}
		return nil
	}
	var resp issueItem
	_ = json.Unmarshal(body, &resp)
	if resp.ID != "" {
		fmt.Fprintf(iio.Stdout, "%s issue %s\n", label, resp.ID)
	} else {
		fmt.Fprintf(iio.Stdout, "%s issue\n", label)
	}
	return nil
}

// wrapAPIError turns a transport / non-2xx error into something the user
// can act on. 401 gets a "token rejected" hint; other statuses surface
// the body snippet for triage.
func wrapAPIError(err error, status int) error {
	if err == nil {
		return nil
	}
	var apiErr *apiclient.APIError
	if errors.As(err, &apiErr) {
		if apiErr.StatusCode == http.StatusUnauthorized {
			return errors.New("token rejected (run sklp auth login)")
		}
		body := string(apiErr.Body)
		if len(body) > 200 {
			body = body[:200] + "…"
		}
		return fmt.Errorf("api %d: %s", apiErr.StatusCode, strings.TrimSpace(body))
	}
	if status == http.StatusUnauthorized {
		return errors.New("token rejected (run sklp auth login)")
	}
	return err
}

func filterIssuesByType(items []issueItem, typ string) []issueItem {
	out := items[:0]
	for _, it := range items {
		if strings.EqualFold(it.Type, typ) {
			out = append(out, it)
		}
	}
	return out
}

func renderIssuesTable(w io.Writer, items []issueItem) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTYPE\tSTATUS\tPROJECT\tTITLE\tTRACKER\tCREATED")
	sort.SliceStable(items, func(i, j int) bool {
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.After(items[j].CreatedAt)
		}
		return items[i].ID < items[j].ID
	})
	for _, it := range items {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			shortID(it.ID),
			emptyDash(it.Type),
			emptyDash(it.Status),
			emptyDash(issueProjectLabel(it)),
			truncateMsg(it.Title, 60),
			emptyDash(it.TrackerProvider),
			it.CreatedAt.Format("2006-01-02"),
		)
	}
	return tw.Flush()
}

func renderIssuesPlain(w io.Writer, items []issueItem) error {
	for _, it := range items {
		title := strings.ReplaceAll(it.Title, "\n", " ")
		title = strings.ReplaceAll(title, "\t", " ")
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			it.ID,
			it.Type,
			strings.ToLower(it.Status),
			issueProjectLabel(it),
			title,
			it.TrackerProvider,
			it.CreatedAt.Format(time.RFC3339),
		)
	}
	return nil
}

func renderIssueShow(w io.Writer, it *issueItem) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "ID:\t%s\n", it.ID)
	fmt.Fprintf(tw, "Project:\t%s\n", issueProjectLabel(*it))
	fmt.Fprintf(tw, "Title:\t%s\n", it.Title)
	fmt.Fprintf(tw, "Type:\t%s\n", emptyDash(it.Type))
	fmt.Fprintf(tw, "Status:\t%s\n", emptyDash(it.Status))
	if it.TrackerProvider != "" {
		fmt.Fprintf(tw, "Tracker:\t%s %s\n", it.TrackerProvider, it.TrackerURL)
	}
	if it.ClosedReason != "" {
		fmt.Fprintf(tw, "Closed reason:\t%s\n", it.ClosedReason)
	}
	if it.StartedAt != nil {
		fmt.Fprintf(tw, "Started:\t%s\n", it.StartedAt.Format(time.RFC3339))
	}
	if it.ClosedAt != nil {
		fmt.Fprintf(tw, "Closed:\t%s\n", it.ClosedAt.Format(time.RFC3339))
	}
	fmt.Fprintf(tw, "Created:\t%s\n", it.CreatedAt.Format(time.RFC3339))
	fmt.Fprintf(tw, "Updated:\t%s\n", it.UpdatedAt.Format(time.RFC3339))
	_ = tw.Flush()
	if it.Body != "" {
		fmt.Fprintln(w)
		fmt.Fprintln(w, it.Body)
	}
}

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func truncateMsg(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

func emptyDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func issueProjectLabel(it issueItem) string {
	if it.ProjectName != "" {
		return it.ProjectName
	}
	if it.ProjectID != "" {
		return shortID(it.ProjectID)
	}
	return ""
}
