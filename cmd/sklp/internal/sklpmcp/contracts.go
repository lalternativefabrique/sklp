package sklpmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lalternativefabrique/sklp/internal/layout"
	"github.com/lalternativefabrique/sklp/internal/repofacts"
	"github.com/lalternativefabrique/sklp/pipeline"
	"github.com/lalternativefabrique/sklp/schema"
)

var guideSkills = map[string]string{
	"dev":      "init-dev",
	"ci":       "init-ci",
	"space":    "init-config",
	"publish":  "init-publish",
	"deploy":   "init-deploy",
	"watches":  "init-watches",
	"operator": "sklp-operator",
}

func (l *local) contractTools() []toolDef {
	return []toolDef{
		define(&mcp.Tool{Name: "sklp_guide", Description: "How to write one sklp contract for THIS repository, tailored to what a scan of it finds " +
			"(languages, toolchain pins, registry, existing contracts): the format, the rules agents get wrong, the commands to check it. " +
			"contract: dev (.sklp/<stack>/dev.yaml), ci (.sklp/pipelines/ci.yaml), space (.sklp/space.yaml), publish (.sklp/pipelines/publish.yaml), " +
			"deploy (.sklp/<stack>/deploy.yaml and .sklp/deploy-root.yaml), watches (.sklp/image-watches.yaml), operator (driving sklp itself). " +
			"Read it before writing the file."}, read, l.guide),
		define(&mcp.Tool{Name: "sklp_draft", Description: "A deterministic first draft of a contract, derived from a scan of the repository " +
			"(contract: dev, ci or space), with notes on what the scan had to guess. Writes nothing: review it, write the file, then validate it " +
			"(contract_validate)."}, read, draft),
		define(&mcp.Tool{Name: "sklp_schema", Description: "The published JSON Schema of a contract, generated from the structs sklp decodes it into: " +
			"pipeline (.sklp/pipelines/<name>.yaml), deploy (.sklp/<stack>/deploy.yaml), deploy-root (.sklp/deploy-root.yaml), " +
			"image-watches (.sklp/image-watches.yaml). Every field it lists exists; nothing else does."}, read, schemaFor),
		define(&mcp.Tool{Name: "contract_validate", Description: "Check a contract you wrote, without running or deploying it: " +
			"dev and pipeline go through the loaders sklp dev and sklp run use; deploy, deploy-root and image-watches are checked against " +
			"their schema and across files (depends_on names, deploy-root paths, a watch's space and service). " +
			"errors make it invalid; warnings are keys the loader ignores, so they never take effect."}, read, l.validateContract),
	}
}

type guideInput struct {
	Repo     string `json:"repo" jsonschema:"path of the repository"`
	Contract string `json:"contract" jsonschema:"dev, ci, space, publish, deploy, watches or operator"`
}

type guideOut struct {
	Contract string `json:"contract"`
	Guide    string `json:"guide"`
}

func (l *local) guide(ctx context.Context, in guideInput, _ *redactor) (guideOut, error) {
	repo, err := repoDir(in.Repo)
	if err != nil {
		return guideOut{}, err
	}
	skill, ok := guideSkills[in.Contract]
	if !ok {
		return guideOut{}, fmt.Errorf("unknown contract %q (want: %s)", in.Contract, strings.Join(sortedKeys(guideSkills), ", "))
	}
	res, err := l.runOK(ctx, repo, "skills", "-repo", repo, "-target", "claude", "-print", skill)
	if err != nil {
		return guideOut{}, err
	}
	return guideOut{Contract: in.Contract, Guide: string(res.stdout)}, nil
}

type draftInput struct {
	Repo     string `json:"repo" jsonschema:"path of the repository"`
	Contract string `json:"contract" jsonschema:"dev, ci or space"`
	Stack    string `json:"stack,omitempty" jsonschema:"for dev: the stack directory under .sklp/ (default stack)"`
}

type draftOut struct {
	Contract string   `json:"contract"`
	Path     string   `json:"path"`
	Exists   bool     `json:"exists"`
	YAML     string   `json:"yaml"`
	Notes    []string `json:"notes"`
}

func draft(_ context.Context, in draftInput, _ *redactor) (draftOut, error) {
	repo, err := repoDir(in.Repo)
	if err != nil {
		return draftOut{}, err
	}
	stack := in.Stack
	if stack == "" {
		stack = layout.DefaultStack
	}
	if strings.ContainsAny(stack, `/\`) || strings.HasPrefix(stack, ".") {
		return draftOut{}, fmt.Errorf("invalid stack name %q", stack)
	}
	out := draftOut{Contract: in.Contract}
	switch in.Contract {
	case "dev":
		out.Path = filepath.Join(".sklp", stack, "dev.yaml")
		out.YAML, out.Notes = repofacts.DevConfig(repo)
	case "ci", "space":
		facts, err := repofacts.Scan(repo)
		if err != nil {
			return draftOut{}, fmt.Errorf("scan %s: %w", repo, err)
		}
		if in.Contract == "ci" {
			rel, _ := filepath.Rel(repo, filepath.Join(layout.New(repo).PipelinesDir(), "ci.yaml"))
			out.Path = rel
			out.YAML, out.Notes = repofacts.CIConfig(facts)
		} else {
			out.Path = filepath.Join(".sklp", "space.yaml")
			out.YAML, out.Notes = repofacts.RunnerConfig(facts)
		}
	default:
		return draftOut{}, fmt.Errorf("unknown contract %q (want: dev, ci, space)", in.Contract)
	}
	_, statErr := os.Stat(filepath.Join(repo, out.Path))
	out.Exists = statErr == nil
	out.Notes = nonNil(out.Notes)
	return out, nil
}

type schemaInput struct {
	Contract string `json:"contract" jsonschema:"pipeline, deploy, deploy-root or image-watches"`
}

type schemaOut struct {
	Contract string         `json:"contract"`
	Schema   map[string]any `json:"schema"`
}

func schemaFor(_ context.Context, in schemaInput, _ *redactor) (schemaOut, error) {
	raw, ok := pipeline.Schema, true
	if in.Contract != "pipeline" {
		raw, ok = schema.Get(in.Contract)
	}
	if !ok {
		return schemaOut{}, fmt.Errorf("unknown contract %q (want: pipeline, deploy, deploy-root, image-watches)", in.Contract)
	}
	out := schemaOut{Contract: in.Contract}
	if err := json.Unmarshal(raw, &out.Schema); err != nil {
		return schemaOut{}, err
	}
	return out, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// addGuidePrompts serves each guide as a prompt too, so a user can load one
// by name (/sklp:init-deploy in Claude Code) instead of waiting for the agent
// to call sklp_guide.
func (l *local) addGuidePrompts(s *mcp.Server) {
	for _, contract := range sortedKeys(guideSkills) {
		skill := guideSkills[contract]
		s.AddPrompt(&mcp.Prompt{
			Name:        skill,
			Description: fmt.Sprintf("The %s guide of a repository, tailored to it, to write or fix its %s contract.", skill, contract),
			Arguments:   []*mcp.PromptArgument{{Name: "repo", Description: "path of the repository (default: the server's working directory)"}},
		}, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			repo := req.Params.Arguments["repo"]
			if repo == "" {
				repo = "."
			}
			g, err := l.guide(ctx, guideInput{Repo: repo, Contract: contract}, nil)
			if err != nil {
				return nil, err
			}
			text := g.Guide + "\n\nWrite or fix the contract from this guide, then run contract_validate and fix it until it reports no error."
			return &mcp.GetPromptResult{
				Description: skill + " for " + repo,
				Messages:    []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: text}}},
			}, nil
		})
	}
}
