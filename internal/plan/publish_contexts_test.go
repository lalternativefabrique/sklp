package plan

import (
	"strings"
	"testing"
)

// TestCompilePublish_BuildContexts_OK verifies the happy path:
// `build_contexts:` flows through compilePublish verbatim into the
// resolved spec, with each (name, value) preserved.
func TestCompilePublish_BuildContexts_OK(t *testing.T) {
	in := CIStepInput{
		Name:    "web",
		Kind:    "publish",
		Image:   "registry.example.com/web",
		Tag:     "v1",
		Context: ".",
		BuildContexts: map[string]string{
			"platform":  "../platform",
			"schemas":   "docker-image://digstack/schemas:v1.2.0",
			"api-stage": "target:api",
		},
	}
	spec, err := compilePublish(in)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(spec.BuildContexts) != 3 {
		t.Fatalf("BuildContexts len = %d, want 3", len(spec.BuildContexts))
	}
	for k, want := range in.BuildContexts {
		if got := spec.BuildContexts[k]; got != want {
			t.Errorf("BuildContexts[%q] = %q, want %q", k, got, want)
		}
	}
}

// TestCompilePublish_BuildContexts_RejectEmpty surfaces typos at
// compile time: a missing source ref must not silently flow through to
// docker (BuildKit would emit a cryptic error 30s later).
func TestCompilePublish_BuildContexts_RejectEmpty(t *testing.T) {
	in := CIStepInput{
		Name:          "web",
		Kind:          "publish",
		Image:         "registry.example.com/web",
		Tag:           "v1",
		Context:       ".",
		BuildContexts: map[string]string{"platform": ""},
	}
	if _, err := compilePublish(in); err == nil {
		t.Fatalf("expected error for empty build_contexts value")
	} else if !strings.Contains(err.Error(), "platform") {
		t.Errorf("error should name the offending key: %v", err)
	}
}
