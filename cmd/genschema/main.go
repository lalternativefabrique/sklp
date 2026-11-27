// Package main generates pipeline/pipeline.schema.json from the
// Go Pipeline struct in the pipeline package.
//
// The reflector is configured to read `yaml:` tags (not `json:`) so the
// schema field names match the keys users actually write in
// `.sklp/pipelines/*.yaml`. Field descriptions are sourced from the Go doc
// comments on the struct fields via Reflector.AddGoComments, which walks
// the pipeline package's source files at generation time.
//
// Invoked via `go generate ./...` from the module
// root. The output is deterministic: same input source ⇒ identical bytes.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/build"
	"os"
	"path/filepath"

	"github.com/invopop/jsonschema"

	"github.com/lalternativefabrique/sklp/pipeline/spec"
	deploydomain "github.com/lalternativefabrique/sklp/reconcile/deployconfig/domain"
	watchdomain "github.com/lalternativefabrique/sklp/reconcile/imageupdate/domain"
	"github.com/lalternativefabrique/sklp/spacemanager"
)

const (
	schemaID    = "https://raw.githubusercontent.com/lalternativefabrique/sklp/main/pipeline/pipeline.schema.json"
	schemaTitle = "sklp pipeline"
	schemaDesc  = "Schema for sklp pipeline files (.sklp/pipelines/*.yaml) generated from the Go Pipeline struct."

	moduleImportPath   = "github.com/lalternativefabrique/sklp"
	pipelineImportPath = moduleImportPath + "/pipeline"
	pipelineSubdir     = "pipeline"

	schemaSubdir  = "schema"
	schemaBaseURL = "https://raw.githubusercontent.com/lalternativefabrique/sklp/main/schema/"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "genschema:", err)
		os.Exit(1)
	}
	if err := runContracts(); err != nil {
		fmt.Fprintln(os.Stderr, "genschema:", err)
		os.Exit(1)
	}
}

func run() error {
	pipelineDir, err := locatePipelineDir()
	if err != nil {
		return err
	}

	r := &jsonschema.Reflector{
		FieldNameTag:               "yaml",
		RequiredFromJSONSchemaTags: false,
		ExpandedStruct:             true,
		DoNotReference:             false,
		// publish.yaml carries fields outside the Pipeline struct
		// (expansion_rules, output, postbuild, ...). Don't reject them —
		// the schema's job is autocomplete + typo detection for the
		// pipeline contract, not gatekeeping the publish one.
		AllowAdditionalProperties: true,
	}
	// AddGoComments builds map keys by joining `base` with each parsed
	// file's `filepath.Dir`. Walking from the module root with
	// path "./pipeline" produces keys shaped like
	// `<moduleImportPath>/pipeline.<Type>.<Field>`, matching what the
	// reflector queries via `t.PkgPath() + "." + t.Name()`.
	moduleRoot := filepath.Dir(pipelineDir)
	prevWD, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}
	if err := os.Chdir(moduleRoot); err != nil {
		return fmt.Errorf("chdir %s: %w", moduleRoot, err)
	}
	commentsErr := r.AddGoComments(moduleImportPath, "./"+pipelineSubdir)
	_ = os.Chdir(prevWD)
	if commentsErr != nil {
		return fmt.Errorf("add go comments: %w", commentsErr)
	}

	s := r.Reflect(&spec.Pipeline{})
	s.ID = schemaID
	s.Version = "https://json-schema.org/draft/2020-12/schema"
	s.Title = schemaTitle
	s.Description = schemaDesc

	// Round-trip through a generic map so we can patch the few spots
	// where the Go type doesn't translate one-to-one to YAML: the
	// custom YAML unmarshallers (string|array) and the legacy `rules`
	// field that real publish.yaml files use with mixed scalars/maps.
	intermediate, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(intermediate, &generic); err != nil {
		return fmt.Errorf("unmarshal intermediate: %w", err)
	}
	patchSchema(generic)

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(generic); err != nil {
		return fmt.Errorf("encode: %w", err)
	}

	out := filepath.Join(pipelineDir, "pipeline.schema.json")
	if err := os.WriteFile(out, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", out, err)
	}
	return nil
}

// patchSchema rewrites the few spots where reflection can't capture the
// effective YAML contract.
func patchSchema(s map[string]any) {
	// ExtendsList / ScopeRef accept either a scalar string or a list of
	// strings in YAML (see schema_extends.go and schema_scope.go).
	stringOrStringArray := map[string]any{
		"oneOf": []any{
			map[string]any{"type": "string"},
			map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
	}
	defs, _ := s["$defs"].(map[string]any)

	// ScanConfigYAML accepts either `scan: false` (bool opt-out) or a
	// mapping — the custom UnmarshalYAML handles both but reflection only
	// sees the struct shape (type: object). Replace with a oneOf.
	if sc, ok := defs["ScanConfigYAML"].(map[string]any); ok {
		desc, _ := sc["description"].(string)
		replacement := map[string]any{
			"oneOf": []any{
				map[string]any{"type": "boolean"},
				map[string]any{
					"type":       "object",
					"properties": sc["properties"],
				},
			},
		}
		if desc != "" {
			replacement["description"] = desc
		}
		defs["ScanConfigYAML"] = replacement
	}

	for _, name := range []string{"ExtendsList", "ScopeRef"} {
		if existing, ok := defs[name].(map[string]any); ok {
			desc, _ := existing["description"].(string)
			replacement := map[string]any{}
			for k, v := range stringOrStringArray {
				replacement[k] = v
			}
			if desc != "" {
				replacement["description"] = desc
			}
			defs[name] = replacement
		}
	}

	// Step is closed: every field a step may carry is on the struct, so a
	// key outside it is a typo that would silently never take effect
	// (`workdir:` for `cwd:`). The root stays open because publish.yaml
	// carries fields outside the Pipeline struct.
	if st, ok := defs["Step"].(map[string]any); ok {
		st["additionalProperties"] = false
	}

	// Legacy `rules` items are free-form: real publish.yaml files mix
	// strings, maps, and multi-line scalars. Loosen to `any` so the
	// schema does not reject them. The same logic applies to the
	// legacy `tasks` block (already `map[string]any`).
	props, _ := s["properties"].(map[string]any)
	if rules, ok := props["rules"].(map[string]any); ok {
		rules["items"] = map[string]any{}
	}

	// StepSecrets is a YAML sequence with two valid shapes selected at
	// parse time (see schema_publish.go::StepSecrets.UnmarshalYAML):
	//   - shell steps:        secrets: [FOO, BAR]
	//   - kind: publish:      secrets: [{id: github_token, env: GITHUB_TOKEN}]
	// Reflection sees the Go struct {Names, Build} and emits a type:
	// object schema that rejects both forms. Replace with a oneOf that
	// matches the actual on-disk shape.
	if ss, ok := defs["StepSecrets"].(map[string]any); ok {
		listOfStrings := map[string]any{
			"type":  "array",
			"items": map[string]any{"type": "string"},
		}
		// BuildSecretYAML is referenced only via StepSecrets.Build (which
		// carries yaml:"-"), so reflection never adds it to $defs. Inline
		// the shape here rather than synthesise a $def — keeps the patch
		// self-contained and matches schema_publish.go::BuildSecretYAML.
		listOfDockerSecrets := map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":   map[string]any{"type": "string"},
					"env":  map[string]any{"type": "string"},
					"file": map[string]any{"type": "string"},
				},
				"required":             []any{"id"},
				"additionalProperties": false,
			},
		}
		replacement := map[string]any{
			"oneOf": []any{listOfStrings, listOfDockerSecrets},
		}
		if desc, ok := ss["description"].(string); ok && desc != "" {
			replacement["description"] = desc
		}
		defs["StepSecrets"] = replacement
	}
}

// locatePipelineDir resolves the on-disk path of the pipeline package so
// AddGoComments can parse its source files. Uses go/build to honour
// GOPATH / module replacement without hard-coding a relative path.
func locatePipelineDir() (string, error) {
	pkg, err := build.Default.Import(pipelineImportPath, ".", build.FindOnly)
	if err != nil {
		return "", fmt.Errorf("locate %s: %w", pipelineImportPath, err)
	}
	return pkg.Dir, nil
}

// contract is a .sklp file whose schema is reflected from the struct its
// loader decodes into. Every object is closed (additionalProperties: false):
// the loaders ignore an unknown key, so the schema is what tells a typo apart.
type contract struct {
	file, title, desc string
	value             any
	commentDirs       []string
	patch             func(root, defs map[string]any)
}

var contracts = []contract{
	{
		file:  "deploy.schema.json",
		title: "sklp deploy",
		desc:  "Schema for .sklp/<stack>/deploy.yaml, generated from spacemanager.DeployConfig. ${VAR} references are expanded before the file is decoded.",
		value: &spacemanager.DeployConfig{}, commentDirs: []string{"./spacemanager", "./ingress"},
		patch: patchDeploy,
	},
	{
		file:  "deploy-root.schema.json",
		title: "sklp deploy-root",
		desc:  "Schema for .sklp/deploy-root.yaml: the spaces a push to the default branch reconciles, generated from domain.Root.",
		value: &deploydomain.Root{}, commentDirs: []string{"./reconcile/deployconfig/domain"},
	},
	{
		file:  "image-watches.schema.json",
		title: "sklp image-watches",
		desc:  "Schema for .sklp/image-watches.yaml: the images a node rolls when the registry gains a matching tag, generated from domain.WatchFile.",
		value: &watchdomain.WatchFile{}, commentDirs: []string{"./reconcile/imageupdate/domain"},
	},
}

func runContracts() error {
	pipelineDir, err := locatePipelineDir()
	if err != nil {
		return err
	}
	moduleRoot := filepath.Dir(pipelineDir)
	for _, c := range contracts {
		r := &jsonschema.Reflector{FieldNameTag: "yaml", ExpandedStruct: true, RequiredFromJSONSchemaTags: true}
		prevWD, err := os.Getwd()
		if err != nil {
			return err
		}
		if err := os.Chdir(moduleRoot); err != nil {
			return err
		}
		for _, dir := range c.commentDirs {
			if err := r.AddGoComments(moduleImportPath, dir); err != nil {
				_ = os.Chdir(prevWD)
				return fmt.Errorf("%s: add go comments: %w", c.file, err)
			}
		}
		_ = os.Chdir(prevWD)

		s := r.Reflect(c.value)
		s.ID = jsonschema.ID(schemaBaseURL + c.file)
		s.Version = "https://json-schema.org/draft/2020-12/schema"
		s.Title = c.title
		s.Description = c.desc
		raw, err := json.Marshal(s)
		if err != nil {
			return err
		}
		var generic map[string]any
		if err := json.Unmarshal(raw, &generic); err != nil {
			return err
		}
		defs, _ := generic["$defs"].(map[string]any)
		if c.patch != nil {
			c.patch(generic, defs)
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		if err := enc.Encode(generic); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(moduleRoot, schemaSubdir, c.file), buf.Bytes(), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// patchDeploy reshapes SpaceSpec, whose UnmarshalYAML reads the mould keys and
// treats every other key as a per-service override.
func patchDeploy(root, defs map[string]any) {
	props, _ := root["properties"].(map[string]any)
	props["version"] = map[string]any{"const": 1, "description": "Version pins the deploy file format; 1 is the only value."}
	props["kind"] = map[string]any{"const": "deploy"}
	props["name"] = map[string]any{"type": "string", "description": "Name labels the stack; the loader does not read it."}
	sp, ok := defs["SpaceSpec"].(map[string]any)
	if !ok {
		return
	}
	spProps, _ := sp["properties"].(map[string]any)
	delete(spProps, "Overrides")
	for goName, key := range map[string]string{
		"Ingress": "ingress", "Gateway": "gateway", "AllowEgress": "allow_egress",
		"Isolate": "isolate", "URL": "url", "TTL": "ttl", "Replicas": "replicas",
	} {
		if v, ok := spProps[goName]; ok {
			delete(spProps, goName)
			spProps[key] = v
		}
	}
	delete(sp, "required")
	sp["additionalProperties"] = map[string]any{"$ref": "#/$defs/ServiceOverride"}
}
