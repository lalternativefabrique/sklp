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
	"github.com/lalternativefabrique/sklp/pipeline/spec"
)

var guideSkills = map[string]string{
	"dev":      "init-dev",
	"ci":       "init-ci",
	"space":    "init-config",
	"publish":  "init-publish",
	"operator": "sklp-operator",
}

func (l *local) contractTools() []toolDef {
	return []toolDef{
		define(&mcp.Tool{Name: "sklp_guide", Description: "How to write one sklp contract for THIS repository, tailored to what a scan of it finds " +
			"(languages, toolchain pins, registry, existing contracts): the format, the rules agents get wrong, the commands to check it. " +
			"contract: dev (.sklp/<stack>/dev.yaml), ci (.sklp/pipelines/ci.yaml), space (.sklp/space.yaml), publish (.sklp/pipelines/publish.yaml), " +
			"operator (driving sklp itself). Read it before writing the file."}, read, l.guide),
		define(&mcp.Tool{Name: "sklp_draft", Description: "A deterministic first draft of a contract, derived from a scan of the repository " +
			"(contract: dev, ci or space), with notes on what the scan had to guess. Writes nothing: review it, write the file, then validate it " +
			"(dev_validate, pipeline_validate)."}, read, draft),
		define(&mcp.Tool{Name: "sklp_schema", Description: "The published JSON Schema of a pipeline file (.sklp/pipelines/<name>.yaml), " +
			"the format the engine loads."}, read, schema),
		define(&mcp.Tool{Name: "pipeline_validate", Description: "Load a pipeline of the repository the way sklp run does, extends: included, " +
			"and report whether it is valid, without running it."}, read, validatePipeline),
	}
}

type guideInput struct {
	Repo     string `json:"repo" jsonschema:"path of the repository"`
	Contract string `json:"contract" jsonschema:"dev, ci, space, publish or operator"`
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

type schemaOut struct {
	Contract string         `json:"contract"`
	Schema   map[string]any `json:"schema"`
}

func schema(_ context.Context, _ noInput, _ *redactor) (schemaOut, error) {
	out := schemaOut{Contract: "pipeline"}
	if err := json.Unmarshal(pipeline.Schema, &out.Schema); err != nil {
		return schemaOut{}, err
	}
	return out, nil
}

type validateOut struct {
	Pipeline string   `json:"pipeline"`
	Valid    bool     `json:"valid"`
	Error    string   `json:"error,omitempty"`
	Steps    []string `json:"steps,omitempty"`
}

func validatePipeline(_ context.Context, in pipelineInput, _ *redactor) (validateOut, error) {
	repo, err := repoDir(in.Repo)
	if err != nil {
		return validateOut{}, err
	}
	lay := layout.New(repo)
	pipelines, err := lay.Pipelines()
	if err != nil {
		return validateOut{}, err
	}
	if !slices.Contains(pipelines, in.Pipeline) {
		return validateOut{}, fmt.Errorf("no pipeline %q in %s (have: %s)", in.Pipeline, repo, strings.Join(pipelines, ", "))
	}
	p, err := spec.LoadPipeline(filepath.Join(lay.PipelinesDir(), in.Pipeline+".yaml"))
	if err != nil {
		return validateOut{Pipeline: in.Pipeline, Error: err.Error()}, nil
	}
	out := validateOut{Pipeline: in.Pipeline, Valid: true}
	for _, s := range head(p.Steps, maxChanges) {
		out.Steps = append(out.Steps, s.Name)
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
