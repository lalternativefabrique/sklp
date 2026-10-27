package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/spacemanager"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
)

func deployDataStoresCommand() cli.Command {
	return cli.Command{
		Name:  "datastores",
		Short: "List the data each space keeps and how well it is protected",
		Flags: append([]cli.Flag{
			{Name: "space", Type: cli.StringFlag, Usage: "space to inspect (empty = every space on the node)"},
			jsonFlag,
		}, netFlagGroup()...),
		Run: func(ctx context.Context, args cli.Args) int {
			return runDeployDataStores(ctx, args.String("space"), netFlagsFromArgs(args), args.Bool("json"))
		},
	}
}

func runDeployDataStores(ctx context.Context, space string, nf *netFlags, asJSON bool) int {
	spaces := []string{space}
	if space == "" {
		var err error
		if spaces, err = nodeSpaces(ctx, nf); err != nil {
			fmt.Fprintln(os.Stderr, "sklp deploy datastores:", err)
			return 1
		}
	}
	var rows []control.DataStoresResponse
	for _, sp := range spaces {
		client, err := nf.dialClient(sp)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sklp deploy datastores: space %q: %v\n", sp, err)
			continue
		}
		res, err := client.DataStores(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "sklp deploy datastores: space %q: %v\n", sp, err)
			continue
		}
		rows = append(rows, res)
	}
	if asJSON {
		return writeJSON(os.Stdout, "sklp deploy datastores", rows)
	}
	renderDataStores(os.Stdout, rows)
	return 0
}

// nodeSpaces enumerates the node's spaces: the local sockets on this host, or
// the node-scoped inventory when talking to a remote node.
func nodeSpaces(ctx context.Context, nf *netFlags) ([]string, error) {
	if !nf.remote() {
		return control.ListSpaces()
	}
	client, err := nf.dialClient("production")
	if err != nil {
		return nil, err
	}
	res, err := client.Spaces(ctx)
	if err != nil {
		return nil, err
	}
	var infos []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(res.Spaces, &infos); err != nil {
		return nil, fmt.Errorf("decode node inventory: %w", err)
	}
	names := make([]string, 0, len(infos))
	for _, i := range infos {
		names = append(names, i.Name)
	}
	return names, nil
}

func renderDataStores(out *os.File, rows []control.DataStoresResponse) {
	counts := map[spacemanager.Protection]int{}
	w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "SPACE\tSERVICE\tENGINE\tPROTECTION\tDETAIL")
	for _, r := range rows {
		for _, s := range r.Stores {
			counts[s.Protection]++
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.Space, s.Service, s.Engine, strings.ToUpper(string(s.Protection)), s.Detail)
		}
	}
	w.Flush()
	fmt.Fprintf(out, "\n%d backed · %d opaque · %d unprotected · %d disposable\n",
		counts[spacemanager.ProtectionBacked], counts[spacemanager.ProtectionOpaque],
		counts[spacemanager.ProtectionNone], counts[spacemanager.ProtectionDisposable])
}
