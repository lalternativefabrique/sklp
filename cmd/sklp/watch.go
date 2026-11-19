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
	"text/tabwriter"
	"time"

	"github.com/lalternativefabrique/sklp/cmd/sklp/internal/apiclient"
	"github.com/lalternativefabrique/sklp/internal/cli"
)

var (
	watchStackFlag = cli.Flag{Name: "stack", Type: cli.StringFlag, EnvFallback: "SKLP_STACK", Usage: "stack name or id (defaults to SKLP_STACK)"}
	watchJSONFlag  = cli.Flag{Name: "json", Type: cli.BoolFlag, Usage: "emit raw JSON from the API"}
)

type watchItem struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Service       string     `json:"service"`
	NodeSpace     string     `json:"node_space"`
	Enabled       bool       `json:"enabled"`
	CurrentTag    string     `json:"current_tag,omitempty"`
	HeldTag       string     `json:"held_tag,omitempty"`
	PinnedTag     string     `json:"pinned_tag,omitempty"`
	Status        string     `json:"status"`
	LastError     string     `json:"last_error,omitempty"`
	LastCheckedAt *time.Time `json:"last_checked_at,omitempty"`
	LastRolledAt  *time.Time `json:"last_rolled_at,omitempty"`
}

type watchRoll struct {
	FromTag      string     `json:"from_tag,omitempty"`
	ToTag        string     `json:"to_tag"`
	Result       string     `json:"result"`
	ErrorMessage string     `json:"error_message,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
}

type stackItem struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func watchCommand() cli.Command {
	run := func(fn func(context.Context, io.Writer, *apiclient.Client, cli.Args) error) func(context.Context, cli.Args) int {
		return func(ctx context.Context, args cli.Args) int {
			client, err := resolveClient()
			if err == nil {
				err = fn(ctx, os.Stdout, client, args)
			}
			if err != nil {
				fmt.Fprintln(os.Stderr, "sklp watch:", err)
				return 1
			}
			return 0
		}
	}
	return cli.Command{
		Name:  "watch",
		Short: "Inspect a stack's image watches (ls | rolls)",
		Long:  "Read-only view of what each image watch last saw and rolled: why a pushed tag is (not) running.",
		Sub: []cli.Command{
			{Name: "ls", Short: "List the stack's watches with status, current/held/pinned tag and last error", Flags: []cli.Flag{watchStackFlag, watchJSONFlag}, Run: run(watchList)},
			{Name: "rolls", Short: "Show a watch's roll history", Flags: []cli.Flag{watchStackFlag, watchJSONFlag, {Name: "limit", Type: cli.IntFlag, Usage: "number of rolls to show (default 20)"}}, Positionals: []cli.Positional{{Name: "watch", Required: true}}, Run: run(watchRolls)},
		},
	}
}

func resolveStackID(ctx context.Context, client *apiclient.Client, ref string) (string, error) {
	if ref == "" {
		return "", errors.New("--stack is required (stack name or id, or set SKLP_STACK)")
	}
	var stacks []stackItem
	if err := getJSON(ctx, client, "/api/stacks", &stacks); err != nil {
		return "", fmt.Errorf("list stacks: %w", err)
	}
	for _, s := range stacks {
		if s.ID == ref || strings.EqualFold(s.Name, ref) {
			return s.ID, nil
		}
	}
	names := make([]string, 0, len(stacks))
	for _, s := range stacks {
		names = append(names, s.Name)
	}
	return "", fmt.Errorf("no stack %q (known: %s)", ref, strings.Join(names, ", "))
}

func getJSON(ctx context.Context, client *apiclient.Client, path string, out any) error {
	body, _, err := client.Get(ctx, path)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

func fetchWatches(ctx context.Context, client *apiclient.Client, stackID string) ([]watchItem, []byte, error) {
	body, _, err := client.Get(ctx, "/api/stacks/"+url.PathEscape(stackID)+"/image-watches")
	if err != nil {
		return nil, nil, err
	}
	var watches []watchItem
	if err := json.Unmarshal(body, &watches); err != nil {
		return nil, nil, fmt.Errorf("decode watches: %w", err)
	}
	return watches, body, nil
}

func watchList(ctx context.Context, out io.Writer, client *apiclient.Client, args cli.Args) error {
	stackID, err := resolveStackID(ctx, client, args.String("stack"))
	if err != nil {
		return err
	}
	watches, raw, err := fetchWatches(ctx, client, stackID)
	if err != nil {
		return err
	}
	if args.Bool("json") {
		_, err := out.Write(append(raw, '\n'))
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATUS\tCURRENT\tHELD\tPINNED\tCHECKED\tLAST ERROR")
	for _, w := range watches {
		status := w.Status
		if !w.Enabled {
			status += " (disabled)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			w.Name, status, dash(w.CurrentTag), dash(w.HeldTag), dash(w.PinnedTag), ago(w.LastCheckedAt), dash(oneLine(w.LastError)))
	}
	return tw.Flush()
}

func watchRolls(ctx context.Context, out io.Writer, client *apiclient.Client, args cli.Args) error {
	stackID, err := resolveStackID(ctx, client, args.String("stack"))
	if err != nil {
		return err
	}
	watches, _, err := fetchWatches(ctx, client, stackID)
	if err != nil {
		return err
	}
	name := args.Positionals()[0]
	var watchID string
	for _, w := range watches {
		if w.Name == name || w.ID == name {
			watchID = w.ID
		}
	}
	if watchID == "" {
		return fmt.Errorf("no watch %q in this stack (run sklp watch ls)", name)
	}
	limit := args.Int("limit")
	if limit <= 0 {
		limit = 20
	}
	body, _, err := client.Get(ctx, fmt.Sprintf("/api/stacks/%s/image-watches/%s/rolls?limit=%d", url.PathEscape(stackID), url.PathEscape(watchID), limit))
	if err != nil {
		return err
	}
	if args.Bool("json") {
		_, err := out.Write(append(body, '\n'))
		return err
	}
	var rolls []watchRoll
	if err := json.Unmarshal(body, &rolls); err != nil {
		return fmt.Errorf("decode rolls: %w", err)
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "STARTED\tFROM\tTO\tRESULT\tERROR")
	for _, r := range rolls {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			r.CreatedAt.UTC().Format("2006-01-02 15:04Z"), dash(r.FromTag), r.ToTag, r.Result, dash(oneLine(r.ErrorMessage)))
	}
	return tw.Flush()
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 120 {
		return s[:117] + "..."
	}
	return s
}

func ago(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return time.Since(*t).Round(time.Minute).String() + " ago"
}
