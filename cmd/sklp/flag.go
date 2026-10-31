// flag.go implements `sklp flag {list,enable,disable}` on top of core's
// per-project feature toggles.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/lalternativefabrique/sklp/cmd/sklp/internal/apiclient"
	"github.com/lalternativefabrique/sklp/internal/cli"
)

type flagItem struct {
	ID          string `json:"id"`
	Key         string `json:"key"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
	UpdatedAt   string `json:"updated_at"`
}

func flagCommand() cli.Command {
	run := func(fn func(*issueIO, cli.Args) error) func(context.Context, cli.Args) int {
		return func(ctx context.Context, args cli.Args) int {
			if err := fn(defaultIssueIO(), args); err != nil {
				fmt.Fprintln(os.Stderr, "sklp flag:", err)
				return 1
			}
			return 0
		}
	}
	set := func(enabled bool) func(context.Context, cli.Args) int {
		return run(func(iio *issueIO, a cli.Args) error { return flagSet(iio, a, enabled) })
	}
	keyPos := []cli.Positional{{Name: "key", Required: true}}
	return cli.Command{
		Name:  "flag",
		Short: "Manage a project's feature flags (list | enable | disable)",
		Sub: []cli.Command{
			{Name: "list", Short: "List the project's feature flags", Flags: flagListFlags(), Run: run(flagList)},
			{Name: "enable", Short: "Turn a feature flag on", Flags: []cli.Flag{issueProjectFlag}, Positionals: keyPos, Run: set(true)},
			{Name: "disable", Short: "Turn a feature flag off", Flags: []cli.Flag{issueProjectFlag}, Positionals: keyPos, Run: set(false)},
		},
	}
}

func flagListFlags() []cli.Flag {
	return []cli.Flag{issueProjectFlag, issueJSONFlag, {Name: "plain", Type: cli.BoolFlag, Usage: "emit TSV (one row per flag, no header)"}}
}

func flagList(iio *issueIO, args cli.Args) error {
	if args.Bool("json") && args.Bool("plain") {
		return errors.New("--json and --plain are mutually exclusive")
	}
	project := pickProject(args.String("project"))
	if project == "" {
		return errors.New("specify --project or set SKLP_PROJECT")
	}
	apic, err := resolveClient()
	if err != nil {
		return err
	}
	body, status, err := apic.Get(context.Background(), flagsPath(project))
	if err != nil {
		return wrapAPIError(err, status)
	}

	if args.Bool("json") {
		if _, err := iio.Stdout.Write(body); err != nil {
			return err
		}
		if len(body) > 0 && body[len(body)-1] != '\n' {
			fmt.Fprintln(iio.Stdout)
		}
		return nil
	}

	var items []flagItem
	if err := json.Unmarshal(body, &items); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Key < items[j].Key })
	if args.Bool("plain") {
		for _, it := range items {
			fmt.Fprintf(iio.Stdout, "%s\t%t\t%s\n", it.Key, it.Enabled, it.Description)
		}
		return nil
	}
	return renderFlagsTable(iio.Stdout, items)
}

func flagSet(iio *issueIO, args cli.Args, enabled bool) error {
	key := firstArg(args, 0)
	if key == "" {
		return errors.New("usage: sklp flag enable|disable <key> [flags]")
	}
	project := pickProject(args.String("project"))
	if project == "" {
		return errors.New("specify --project or set SKLP_PROJECT")
	}
	apic, err := resolveClient()
	if err != nil {
		return err
	}
	id, err := lookupFlagID(apic, project, key)
	if err != nil {
		return err
	}
	payload := map[string]any{"enabled": enabled}
	if _, status, err := apic.Patch(context.Background(), flagsPath(project)+"/"+url.PathEscape(id), payload); err != nil {
		return wrapAPIError(err, status)
	}
	state := "disabled"
	if enabled {
		state = "enabled"
	}
	fmt.Fprintf(iio.Stdout, "%s: %s\n", key, state)
	return nil
}

func lookupFlagID(apic *apiclient.Client, project, key string) (string, error) {
	body, status, err := apic.Get(context.Background(), flagsPath(project))
	if err != nil {
		return "", wrapAPIError(err, status)
	}
	var items []flagItem
	if err := json.Unmarshal(body, &items); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}
	for _, it := range items {
		if it.Key == key {
			return it.ID, nil
		}
	}
	return "", fmt.Errorf("flag %s not found in project %s", key, project)
}

func flagsPath(project string) string {
	return "/api/projects/" + url.PathEscape(project) + "/toggles"
}

func renderFlagsTable(w io.Writer, items []flagItem) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "KEY\tENABLED\tDESCRIPTION\tUPDATED")
	for _, it := range items {
		fmt.Fprintf(tw, "%s\t%t\t%s\t%s\n", it.Key, it.Enabled, emptyDash(it.Description), it.UpdatedAt)
	}
	return tw.Flush()
}
