package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/lalternativefabrique/sklp/cmd/sklp/internal/apiclient"
	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/internal/config"
	"github.com/lalternativefabrique/sklp/internal/layout"
	"github.com/lalternativefabrique/sklp/internal/stackconfig"
)

const whoamiAPITimeout = 10 * time.Second

type whoamiReport struct {
	API        whoamiAPI       `json:"api"`
	Projects   []projectItem   `json:"projects"`
	Contexts   []whoamiContext `json:"contexts"`
	Repo       whoamiRepo      `json:"repo"`
	Passphrase string          `json:"secrets_passphrase_source"`
}

type whoamiAPI struct {
	URL      string `json:"url"`
	LoggedIn bool   `json:"logged_in"`
	Email    string `json:"email,omitempty"`
	Error    string `json:"error,omitempty"`
}

type whoamiContext struct {
	Name    string `json:"name"`
	Server  string `json:"server"`
	Space   string `json:"space,omitempty"`
	Current bool   `json:"current"`
}

type whoamiRepo struct {
	Root      string        `json:"root"`
	Stacks    []whoamiStack `json:"stacks"`
	Pipelines []string      `json:"pipelines"`
}

type whoamiStack struct {
	Name      string      `json:"name"`
	Contracts []string    `json:"contracts"`
	Config    []whoamiKey `json:"config,omitempty"`
}

type whoamiKey struct {
	Name   string `json:"name"`
	Secret bool   `json:"secret"`
}

func whoamiCommand() cli.Command {
	return cli.Command{
		Name:  "whoami",
		Short: "Everything an operator or an AI agent needs before acting: API login, projects, deploy contexts, stacks and config keys",
		Long: "One call answers the questions that otherwise need five: who am I logged in as,\n" +
			"which projects and stacks exist (with their ids), which deploy contexts point\n" +
			"where, what this repo declares under .sklp/, and where the secrets passphrase\n" +
			"comes from. No secret value is ever printed. -json is the form for agents.",
		Flags: []cli.Flag{
			{Name: "repo", Type: cli.StringFlag, StringDefault: ".", Usage: "repository root to inspect"},
			issueJSONFlag,
		},
		Run: func(ctx context.Context, args cli.Args) int {
			rep := buildWhoami(ctx, args.String("repo"))
			if args.Bool("json") {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				if err := enc.Encode(rep); err != nil {
					fmt.Fprintln(os.Stderr, "sklp whoami:", err)
					return 1
				}
				return 0
			}
			renderWhoami(os.Stdout, rep)
			return 0
		},
	}
}

func buildWhoami(ctx context.Context, repo string) whoamiReport {
	rep := whoamiReport{Projects: []projectItem{}, Contexts: []whoamiContext{}}
	cfg, _, err := config.Load()
	if err != nil {
		rep.API.Error = "load config: " + err.Error()
	} else {
		rep.API, rep.Projects = whoamiAPIAndProjects(ctx, cfg)
		rep.Contexts = whoamiContexts(cfg)
	}
	rep.Repo = whoamiRepoFacts(repo)
	_, src, perr := stackconfig.ResolvePassphrase(repo)
	rep.Passphrase = string(src)
	if perr != nil {
		rep.Passphrase = string(stackconfig.SourceNone) + " (" + perr.Error() + ")"
	}
	return rep
}

func whoamiAPIAndProjects(ctx context.Context, cfg *config.Config) (whoamiAPI, []projectItem) {
	api, err := cfg.ResolveAPI()
	if err != nil {
		return whoamiAPI{Error: err.Error()}, nil
	}
	out := whoamiAPI{URL: api.URL}
	token := config.EffectiveAPIToken(api)
	if token == "" {
		out.Error = "not logged in (run sklp auth login)"
		return out, nil
	}
	ctx, cancel := context.WithTimeout(ctx, whoamiAPITimeout)
	defer cancel()
	client := apiclient.New(api.URL, token)
	body, status, err := client.Get(ctx, "/api/me")
	if err != nil {
		out.Error = wrapAPIError(err, status).Error()
		return out, nil
	}
	var me meResponse
	_ = json.Unmarshal(body, &me)
	out.LoggedIn = true
	out.Email = me.Email

	body, status, err = client.Get(ctx, "/api/projects")
	if err != nil {
		out.Error = "projects: " + wrapAPIError(err, status).Error()
		return out, nil
	}
	var items []projectItem
	if err := json.Unmarshal(body, &items); err != nil {
		out.Error = "projects: parse response: " + err.Error()
		return out, nil
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return out, items
}

func whoamiContexts(cfg *config.Config) []whoamiContext {
	names := make([]string, 0, len(cfg.Contexts))
	for name := range cfg.Contexts {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]whoamiContext, 0, len(names))
	for _, name := range names {
		c := cfg.Contexts[name]
		out = append(out, whoamiContext{Name: name, Server: c.Server, Space: c.Space, Current: name == cfg.CurrentContext})
	}
	return out
}

func whoamiRepoFacts(repo string) whoamiRepo {
	root, err := filepath.Abs(repo)
	if err != nil {
		root = repo
	}
	out := whoamiRepo{Root: root, Stacks: []whoamiStack{}, Pipelines: []string{}}
	lay := layout.New(repo)
	stacks, err := lay.Stacks()
	if err != nil {
		return out
	}
	for _, name := range stacks {
		st := whoamiStack{Name: name, Contracts: []string{}}
		if verbs, err := lay.Verbs(name); err == nil {
			for _, v := range verbs {
				if v != "config" {
					st.Contracts = append(st.Contracts, v)
				}
			}
		}
		st.Config = whoamiConfigKeys(filepath.Join(root, ".sklp", name, "config.yaml"))
		out.Stacks = append(out.Stacks, st)
	}
	if names, err := lay.Pipelines(); err == nil {
		out.Pipelines = names
	}
	return out
}

func whoamiConfigKeys(path string) []whoamiKey {
	sc, err := stackconfig.Load(path)
	if err != nil {
		return nil
	}
	keys := sc.Keys()
	out := make([]whoamiKey, 0, len(keys))
	for _, k := range keys {
		out = append(out, whoamiKey{Name: k, Secret: sc.IsSecret(k)})
	}
	return out
}

func renderWhoami(w io.Writer, rep whoamiReport) {
	switch {
	case rep.API.LoggedIn:
		fmt.Fprintf(w, "api        %s · %s\n", rep.API.Email, rep.API.URL)
	case rep.API.URL != "":
		fmt.Fprintf(w, "api        %s · %s\n", rep.API.URL, rep.API.Error)
	default:
		fmt.Fprintf(w, "api        %s\n", rep.API.Error)
	}
	fmt.Fprintf(w, "secrets    passphrase from %s\n", rep.Passphrase)
	fmt.Fprintf(w, "repo       %s\n\n", rep.Repo.Root)

	if len(rep.Projects) > 0 {
		fmt.Fprintln(w, "PROJECTS")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  ID\tNAME\tSTACK\tREPO")
		for _, p := range rep.Projects {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", p.ID, p.Name, emptyDash(p.StackID), emptyDash(p.GithubRepo))
		}
		_ = tw.Flush()
		fmt.Fprintln(w)
	}

	if len(rep.Contexts) > 0 {
		fmt.Fprintln(w, "DEPLOY CONTEXTS")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  CURRENT\tNAME\tSERVER\tSPACE")
		for _, c := range rep.Contexts {
			mark := ""
			if c.Current {
				mark = "*"
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", mark, c.Name, c.Server, emptyDash(c.Space))
		}
		_ = tw.Flush()
		fmt.Fprintln(w)
	}

	if len(rep.Repo.Stacks) > 0 {
		fmt.Fprintln(w, "STACKS (.sklp/<stack>/)")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  STACK\tCONTRACTS\tCONFIG KEYS")
		for _, s := range rep.Repo.Stacks {
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", s.Name, emptyDash(strings.Join(s.Contracts, ",")), emptyDash(formatWhoamiKeys(s.Config)))
		}
		_ = tw.Flush()
		fmt.Fprintln(w)
	}
	if len(rep.Repo.Pipelines) > 0 {
		fmt.Fprintf(w, "PIPELINES  %s\n", strings.Join(rep.Repo.Pipelines, ", "))
	}
}

func formatWhoamiKeys(keys []whoamiKey) string {
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if k.Secret {
			parts = append(parts, k.Name+"🔒")
			continue
		}
		parts = append(parts, k.Name)
	}
	return strings.Join(parts, ",")
}
