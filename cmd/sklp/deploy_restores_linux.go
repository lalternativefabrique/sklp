package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/restore/drill"
)

func deployRestoresCommand() cli.Command {
	return cli.Command{
		Name:  "restores",
		Short: "List the node's restore drills with the latest verdict of each",
		Flags: append([]cli.Flag{jsonFlag}, netFlagGroup()...),
		Run: func(ctx context.Context, args cli.Args) int {
			return runDeployRestores(ctx, netFlagsFromArgs(args), args.Bool("json"))
		},
	}
}

type restoreTargetRow struct {
	Name    string            `json:"name"`
	Engine  string            `json:"engine"`
	Target  string            `json:"target"`
	History []json.RawMessage `json:"history"`
}

func runDeployRestores(ctx context.Context, nf *netFlags, asJSON bool) int {
	client, err := nf.dialClient("production")
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp deploy restores:", err)
		return 1
	}
	res, err := client.Restores(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp deploy restores:", err)
		return 1
	}
	if asJSON {
		return writeJSON(os.Stdout, "sklp deploy restores", res.Targets)
	}
	var targets []restoreTargetRow
	if err := json.Unmarshal(res.Targets, &targets); err != nil {
		fmt.Fprintln(os.Stderr, "sklp deploy restores: decode:", err)
		return 1
	}
	if len(targets) == 0 {
		fmt.Println("no restore drill has reported on this node yet")
		return 0
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "TARGET\tENGINE\tLAST RUN\tSTATUS\tRTO\tRPO\tROWS\tNIGHTS OK")
	for _, t := range targets {
		var latest drill.Report
		if len(t.History) == 0 || json.Unmarshal(t.History[0], &latest) != nil {
			continue
		}
		var rows int64
		for _, c := range latest.Inventory.Collections {
			rows += c.Count
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\n", t.Name, t.Engine,
			latest.FinishedAt.Local().Format(time.DateTime), strings.ToUpper(string(latest.Status)),
			latest.RTO, latest.RPO, rows, nightsOK(t.History))
	}
	w.Flush()
	return 0
}

func nightsOK(history []json.RawMessage) string {
	ok := 0
	for _, raw := range history {
		var r drill.Report
		if json.Unmarshal(raw, &r) == nil && r.Status == drill.StatusOK {
			ok++
		}
	}
	return fmt.Sprintf("%d/%d", ok, len(history))
}
