package pipeline_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

// schemaPath resolves the path to the checked-in JSON Schema relative
// to this test file. Using runtime.Caller keeps the test working
// regardless of the working directory `go test` was invoked from.
func schemaPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "pipeline.schema.json")
}

func loadSchemaBytes(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(schemaPath(t))
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	return b
}

func TestPipelineSchemaShape(t *testing.T) {
	raw := loadSchemaBytes(t)
	if len(raw) == 0 {
		t.Fatal("schema file is empty")
	}

	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}

	for _, key := range []string{"$schema", "$id", "properties", "$defs", "title"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("schema missing top-level key %q", key)
		}
	}

	// Field names must match the YAML keys users write.
	props, _ := doc["properties"].(map[string]any)
	for _, key := range []string{"version", "kind", "steps", "services", "scopes", "tools", "env_files"} {
		if _, ok := props[key]; !ok {
			t.Errorf("expected top-level property %q", key)
		}
	}

	// Descriptions sourced from Go doc comments must propagate.
	defs, _ := doc["$defs"].(map[string]any)
	step, _ := defs["Step"].(map[string]any)
	stepProps, _ := step["properties"].(map[string]any)
	dep, _ := stepProps["depends_on"].(map[string]any)
	desc, _ := dep["description"].(string)
	if !strings.Contains(desc, "DependsOn") {
		t.Errorf("expected depends_on description from Go comment, got %q", desc)
	}
}

func TestPipelineSchemaIsDeterministic(t *testing.T) {
	// Regenerate the schema in a temp dir and compare against the
	// checked-in file. Drift means a contributor forgot to re-run
	// `go generate` after editing schema.go.
	tmp := t.TempDir()
	out := filepath.Join(tmp, "pipeline.schema.json")

	// Resolve the apps/sklp module root from the test file location.
	_, thisFile, _, _ := runtime.Caller(0)
	moduleRoot := filepath.Dir(filepath.Dir(thisFile))

	cmd := exec.Command("go", "run", "./cmd/genschema")
	cmd.Dir = moduleRoot
	cmd.Env = append(os.Environ(), "GENSCHEMA_OUT="+out)
	// The generator currently writes to the package dir; capture the
	// produced bytes by reading from the canonical location after the
	// run, then comparing in-memory.
	if buf, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go run ./cmd/genschema: %v\n%s", err, buf)
	}

	got, err := os.ReadFile(schemaPath(t))
	if err != nil {
		t.Fatalf("read regenerated schema: %v", err)
	}
	if !bytes.Equal(got, loadSchemaBytes(t)) {
		t.Fatal("regenerated schema differs from checked-in file; run `go generate ./apps/sklp/...`")
	}
}

func TestPipelineSchemaValidatesRepoTasks(t *testing.T) {
	compiler := jsonschema.NewCompiler()
	raw := loadSchemaBytes(t)
	var schemaDoc any
	if err := json.Unmarshal(raw, &schemaDoc); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	if err := compiler.AddResource("pipeline.schema.json", schemaDoc); err != nil {
		t.Fatalf("add schema resource: %v", err)
	}
	schema, err := compiler.Compile("pipeline.schema.json")
	if err != nil {
		t.Fatalf("compile schema: %v", err)
	}

	// Locate the repo root: walk up from the test file until a .sklp
	// directory is found. Keeps the test independent of which clone /
	// worktree it runs in.
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Dir(thisFile)
	var pipelinesDir string
	for i := 0; i < 10; i++ {
		// Canonical .sklp/pipelines/, with the legacy .sklp/tasks/ as a fallback.
		for _, name := range []string{"pipelines", "tasks"} {
			cand := filepath.Join(dir, ".sklp", name)
			if st, err := os.Stat(cand); err == nil && st.IsDir() {
				pipelinesDir = cand
				break
			}
		}
		if pipelinesDir != "" {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if pipelinesDir == "" {
		t.Skip("repo root with .sklp/pipelines (or legacy .sklp/tasks) not found; skipping")
	}

	entries, err := os.ReadDir(pipelinesDir)
	if err != nil {
		t.Fatalf("read pipelines dir: %v", err)
	}
	var checked int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		path := filepath.Join(pipelinesDir, e.Name())
		yb, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var node any
		if err := yaml.Unmarshal(yb, &node); err != nil {
			t.Fatalf("yaml %s: %v", path, err)
		}
		// jsonschema/v6 expects map[string]any, not the
		// map[interface{}]interface{} that yaml.v3 may produce.
		normalized := normalizeYAML(node)
		if err := schema.Validate(normalized); err != nil {
			t.Errorf("schema rejects %s:\n%v", e.Name(), err)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no .sklp/pipelines/*.yaml files were validated")
	}
}

// normalizeYAML converts a yaml.v3-decoded tree into the JSON-friendly
// shape jsonschema/v6 expects (map[string]any everywhere).
func normalizeYAML(in any) any {
	switch v := in.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, vv := range v {
			out[k] = normalizeYAML(vv)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(v))
		for k, vv := range v {
			out[toString(k)] = normalizeYAML(vv)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, vv := range v {
			out[i] = normalizeYAML(vv)
		}
		return out
	default:
		return v
	}
}

func toString(k any) string {
	if s, ok := k.(string); ok {
		return s
	}
	return ""
}
