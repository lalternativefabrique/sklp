// project.go implements `sklp project list` — lists the caller's projects,
// each with the stack it belongs to. This is the discovery step for -project
// / SKLP_PROJECT: today there's no way to learn a project id short of the UI.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/lalternativefabrique/sklp/internal/cli"
)

// projectItem mirrors ProjectListItem (the server's ProjectListItem DTO). Unknown
// fields are tolerated — we only map what we render.
type projectItem struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	GithubRepo string    `json:"github_repo,omitempty"`
	StackID    string    `json:"stack_id,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

func projectCommand() cli.Command {
	run := func(fn func(*issueIO, cli.Args) error) func(context.Context, cli.Args) int {
		return func(ctx context.Context, args cli.Args) int {
			if err := fn(defaultIssueIO(), args); err != nil {
				fmt.Fprintln(os.Stderr, "sklp project:", err)
				return 1
			}
			return 0
		}
	}
	return cli.Command{
		Name:  "project",
		Short: "Manage projects (list)",
		Sub: []cli.Command{
			{Name: "list", Short: "List your projects", Flags: projectListFlags(), Run: run(projectList)},
		},
	}
}

func projectListFlags() []cli.Flag {
	return []cli.Flag{
		{Name: "stack", Type: cli.StringFlag, Usage: "filter by stack id"},
		issueJSONFlag,
		{Name: "plain", Type: cli.BoolFlag, Usage: "emit TSV (one row per project, no header)"},
	}
}

func projectList(iio *issueIO, args cli.Args) error {
	if args.Bool("json") && args.Bool("plain") {
		return fmt.Errorf("--json and --plain are mutually exclusive")
	}

	cli, err := resolveClient()
	if err != nil {
		return err
	}

	body, status, err := cli.Get(context.Background(), "/api/projects")
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

	var items []projectItem
	if err := json.Unmarshal(body, &items); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	if stackFlag := args.String("stack"); stackFlag != "" {
		items = filterProjectsByStack(items, stackFlag)
	}

	if args.Bool("plain") {
		return renderProjectsPlain(iio.Stdout, items)
	}
	return renderProjectsTable(iio.Stdout, items)
}

func filterProjectsByStack(items []projectItem, stackID string) []projectItem {
	out := items[:0]
	for _, it := range items {
		if it.StackID == stackID || strings.HasPrefix(it.StackID, stackID) {
			out = append(out, it)
		}
	}
	return out
}

func renderProjectsTable(w io.Writer, items []projectItem) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSTACK\tCREATED")
	sort.SliceStable(items, func(i, j int) bool {
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.After(items[j].CreatedAt)
		}
		return items[i].ID < items[j].ID
	})
	for _, it := range items {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
			it.ID,
			it.Name,
			emptyDash(it.StackID),
			it.CreatedAt.Format("2006-01-02"),
		)
	}
	return tw.Flush()
}

func renderProjectsPlain(w io.Writer, items []projectItem) error {
	for _, it := range items {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			it.ID,
			it.Name,
			it.StackID,
			it.CreatedAt.Format(time.RFC3339),
		)
	}
	return nil
}
