package sklpmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	schemakind "github.com/santhosh-tekuri/jsonschema/v6/kind"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
	"gopkg.in/yaml.v3"

	"github.com/lalternativefabrique/sklp/internal/layout"
	"github.com/lalternativefabrique/sklp/pipeline/spec"
	deploydomain "github.com/lalternativefabrique/sklp/reconcile/deployconfig/domain"
	watchdomain "github.com/lalternativefabrique/sklp/reconcile/imageupdate/domain"
	"github.com/lalternativefabrique/sklp/schema"
)

const maxProblems = 50

type validateInput struct {
	Repo     string `json:"repo" jsonschema:"path of the repository"`
	Contract string `json:"contract" jsonschema:"dev, deploy, pipeline, deploy-root or image-watches"`
	Stack    string `json:"stack,omitempty" jsonschema:"for dev and deploy: the stack under .sklp/ (see stacks)"`
	Pipeline string `json:"pipeline,omitempty" jsonschema:"for pipeline: its name, the <name> of .sklp/pipelines/<name>.yaml"`
}

type validateOut struct {
	Contract string   `json:"contract"`
	File     string   `json:"file"`
	Valid    bool     `json:"valid"`
	Errors   []string `json:"errors,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
	Steps    []string `json:"steps,omitempty"`
}

func (l *local) validateContract(ctx context.Context, in validateInput, _ *redactor) (validateOut, error) {
	repo, err := repoDir(in.Repo)
	if err != nil {
		return validateOut{}, err
	}
	out := validateOut{Contract: in.Contract}
	switch in.Contract {
	case "dev":
		if in.Stack == "" {
			return validateOut{}, errors.New("stack is required for dev (see stacks)")
		}
		out.File = filepath.Join(".sklp", in.Stack, "dev.yaml")
		res, err := l.run(ctx, repo, quickTimeout, "dev", in.Stack, "-validate", "-check", "-no-color", "-repo", repo)
		if err != nil {
			return validateOut{}, err
		}
		if res.exitCode != 0 {
			out.Errors = res.output
		} else {
			out.Warnings = res.output
		}
	case "pipeline":
		if err := validatePipelineFile(repo, in.Pipeline, &out); err != nil {
			return validateOut{}, err
		}
	case "deploy":
		if in.Stack == "" {
			return validateOut{}, errors.New("stack is required for deploy (see stacks)")
		}
		out.File = filepath.Join(".sklp", in.Stack, "deploy.yaml")
		validateDeployFile(filepath.Join(repo, out.File), &out)
	case "deploy-root":
		out.File = deploydomain.RootPath
		validateDeployRoot(repo, &out)
	case "image-watches":
		out.File = watchdomain.WatchFilePath
		validateWatches(repo, &out)
	default:
		return validateOut{}, fmt.Errorf("unknown contract %q (want: dev, deploy, pipeline, deploy-root, image-watches)", in.Contract)
	}
	out.Valid = len(out.Errors) == 0
	out.Errors, out.Warnings = head(out.Errors, maxProblems), head(out.Warnings, maxProblems)
	return out, nil
}

func validatePipelineFile(repo, name string, out *validateOut) error {
	lay := layout.New(repo)
	pipelines, err := lay.Pipelines()
	if err != nil {
		return err
	}
	if !slices.Contains(pipelines, name) {
		return fmt.Errorf("no pipeline %q in %s (have: %s)", name, repo, strings.Join(pipelines, ", "))
	}
	path := filepath.Join(lay.PipelinesDir(), name+".yaml")
	out.File, _ = filepath.Rel(repo, path)
	p, err := spec.LoadPipeline(path)
	if err != nil {
		out.Errors = []string{err.Error()}
		return nil
	}
	for _, s := range head(p.Steps, maxChanges) {
		out.Steps = append(out.Steps, s.Name)
	}
	return nil
}

type deployShape struct {
	Services map[string]struct {
		Image     string   `yaml:"image"`
		DependsOn []string `yaml:"depends_on"`
	} `yaml:"services"`
	Jobs map[string]struct {
		Image     string   `yaml:"image"`
		DependsOn []string `yaml:"depends_on"`
	} `yaml:"jobs"`
}

func (d deployShape) names() map[string]bool {
	out := map[string]bool{}
	for n := range d.Services {
		out[n] = true
	}
	for n := range d.Jobs {
		out[n] = true
	}
	return out
}

func validateDeployFile(path string, out *validateOut) *deployShape {
	raw, err := os.ReadFile(path)
	if err != nil {
		out.Errors = append(out.Errors, err.Error())
		return nil
	}
	if !checkSchema("deploy", raw, out) {
		return nil
	}
	var d deployShape
	if err := yaml.Unmarshal(raw, &d); err != nil {
		out.Errors = append(out.Errors, err.Error())
		return nil
	}
	names := d.names()
	if len(names) == 0 {
		out.Errors = append(out.Errors, "the file declares no services or jobs")
	}
	if len(names) < len(d.Services)+len(d.Jobs) {
		out.Errors = append(out.Errors, "a name is used by both a service and a job: names are unique across both")
	}
	check := func(family, name, image string, deps []string) {
		if strings.TrimSpace(image) == "" {
			out.Errors = append(out.Errors, fmt.Sprintf("%s.%s: image is required", family, name))
		}
		for _, dep := range deps {
			if !names[dep] {
				out.Errors = append(out.Errors, fmt.Sprintf("%s.%s: depends_on %q is neither a service nor a job of this file", family, name, dep))
			}
		}
	}
	for _, n := range sortedNames(d.Services) {
		check("services", n, d.Services[n].Image, d.Services[n].DependsOn)
	}
	for _, n := range sortedNames(d.Jobs) {
		check("jobs", n, d.Jobs[n].Image, d.Jobs[n].DependsOn)
	}
	return &d
}

func validateDeployRoot(repo string, out *validateOut) map[string]string {
	raw, err := os.ReadFile(filepath.Join(repo, deploydomain.RootPath))
	if err != nil {
		out.Errors = append(out.Errors, err.Error())
		return nil
	}
	checkSchema("deploy-root", raw, out)
	root, err := deploydomain.ParseRoot(raw)
	if err != nil {
		out.Errors = append(out.Errors, err.Error())
		return nil
	}
	spaces := map[string]string{}
	for _, d := range root.Deployments {
		if prev, dup := spaces[d.Space]; dup {
			out.Errors = append(out.Errors, fmt.Sprintf("space %q is deployed twice (%s and %s)", d.Space, prev, d.Path))
		}
		spaces[d.Space] = d.Path
		if _, err := os.Stat(filepath.Join(repo, d.Path)); err != nil {
			out.Errors = append(out.Errors, fmt.Sprintf("space %q: path %s does not exist in the repo", d.Space, d.Path))
		}
	}
	return spaces
}

func validateWatches(repo string, out *validateOut) {
	raw, err := os.ReadFile(filepath.Join(repo, watchdomain.WatchFilePath))
	if err != nil {
		out.Errors = append(out.Errors, err.Error())
		return
	}
	checkSchema("image-watches", raw, out)
	f, err := watchdomain.ParseWatchFile(raw)
	if err != nil {
		out.Errors = append(out.Errors, err.Error())
		return
	}
	var spaces map[string]string
	if _, err := os.Stat(filepath.Join(repo, deploydomain.RootPath)); err == nil {
		var rootOut validateOut
		spaces = validateDeployRoot(repo, &rootOut)
	}
	seen := map[string]bool{}
	for i, w := range f.Watches {
		label := fmt.Sprintf("watches[%d]", i)
		if w.Name != "" {
			label += " " + w.Name
		}
		if seen[w.Name] && w.Name != "" {
			out.Errors = append(out.Errors, label+": name is declared twice")
		}
		seen[w.Name] = true
		for _, p := range w.Problems() {
			out.Errors = append(out.Errors, label+": "+p)
		}
		if spaces == nil || w.Space == "" {
			continue
		}
		path, ok := spaces[w.Space]
		if !ok {
			out.Warnings = append(out.Warnings, fmt.Sprintf("%s: space %q is not one deploy-root.yaml deploys", label, w.Space))
			continue
		}
		var d deployShape
		if b, err := os.ReadFile(filepath.Join(repo, path)); err == nil && yaml.Unmarshal(b, &d) == nil && !d.names()[w.Service] {
			out.Errors = append(out.Errors, fmt.Sprintf("%s: service %q is not in %s, which deploys space %q", label, w.Service, path, w.Space))
		}
	}
}

// checkSchema validates raw against the contract's schema. A key the schema
// does not know is a warning: the loader ignores it, so it never takes
// effect. A type error on a ${VAR} value is skipped: it is expanded first.
func checkSchema(contract string, raw []byte, out *validateOut) bool {
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		out.Errors = append(out.Errors, "not valid YAML: "+err.Error())
		return false
	}
	sch, err := compiledSchema(contract)
	if err != nil {
		out.Errors = append(out.Errors, err.Error())
		return false
	}
	inst, err := jsonInstance(doc)
	if err != nil {
		out.Errors = append(out.Errors, err.Error())
		return false
	}
	verr, ok := sch.Validate(inst).(*jsonschema.ValidationError)
	if !ok {
		return true
	}
	p := message.NewPrinter(language.English)
	for _, leaf := range leaves(verr) {
		at := "/" + strings.Join(leaf.InstanceLocation, "/")
		if extra, ok := leaf.ErrorKind.(*schemakind.AdditionalProperties); ok {
			for _, key := range extra.Properties {
				out.Warnings = append(out.Warnings, fmt.Sprintf("%s: unknown key %q is ignored, so it has no effect", at, key))
			}
			continue
		}
		if v, ok := valueAt(inst, leaf.InstanceLocation).(string); ok && strings.Contains(v, "${") {
			continue
		}
		out.Errors = append(out.Errors, at+": "+leaf.ErrorKind.LocalizedString(p))
	}
	return true
}

func compiledSchema(contract string) (*jsonschema.Schema, error) {
	raw, ok := schema.Get(contract)
	if !ok {
		return nil, fmt.Errorf("no schema for %s", contract)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(contract+".json", doc); err != nil {
		return nil, err
	}
	return c.Compile(contract + ".json")
}

func jsonInstance(v any) (any, error) {
	raw, err := json.Marshal(stringKeys(v))
	if err != nil {
		return nil, err
	}
	var out any
	return out, json.Unmarshal(raw, &out)
}

func stringKeys(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			t[k] = stringKeys(x)
		}
		return t
	case map[any]any:
		m := make(map[string]any, len(t))
		for k, x := range t {
			m[fmt.Sprint(k)] = stringKeys(x)
		}
		return m
	case []any:
		for i, x := range t {
			t[i] = stringKeys(x)
		}
	}
	return v
}

func leaves(e *jsonschema.ValidationError) []*jsonschema.ValidationError {
	if len(e.Causes) == 0 {
		return []*jsonschema.ValidationError{e}
	}
	var out []*jsonschema.ValidationError
	for _, c := range e.Causes {
		out = append(out, leaves(c)...)
	}
	return out
}

func valueAt(v any, path []string) any {
	for _, key := range path {
		switch t := v.(type) {
		case map[string]any:
			v = t[key]
		case []any:
			var i int
			if _, err := fmt.Sscan(key, &i); err != nil || i < 0 || i >= len(t) {
				return nil
			}
			v = t[i]
		default:
			return nil
		}
	}
	return v
}

func sortedNames[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
