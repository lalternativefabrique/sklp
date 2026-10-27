package pipeline

import (
	"os"
	"strings"
	"testing"
)

func TestPrependPath_NoExistingPATH(t *testing.T) {
	env := []string{"FOO=bar"}
	got := prependPath(env, []string{"/x"})
	want := "PATH=/x"
	for _, kv := range got {
		if kv == want {
			return
		}
	}
	t.Errorf("PATH=%q not found in env %v", want, got)
}

func TestPrependPath_ExistingPATH(t *testing.T) {
	env := []string{"PATH=/usr/bin:/bin", "FOO=bar"}
	got := prependPath(env, []string{"/sklp/go", "/sklp/kubectl"})
	for _, kv := range got {
		if strings.HasPrefix(kv, "PATH=") {
			want := "PATH=/sklp/go" + string(os.PathListSeparator) + "/sklp/kubectl" + string(os.PathListSeparator) + "/usr/bin:/bin"
			if kv != want {
				t.Errorf("PATH = %q, want %q", kv, want)
			}
			return
		}
	}
	t.Fatal("PATH var not preserved")
}

func TestPrependPath_EmptyDirs(t *testing.T) {
	env := []string{"PATH=/usr/bin"}
	got := prependPath(env, nil)
	if len(got) != 1 || got[0] != "PATH=/usr/bin" {
		t.Errorf("env mutated unexpectedly: %v", got)
	}
}

// installTools was retired in favour of toolResolver — install is now
// lazy and per-step. The "no tools declared" branch is exercised
// implicitly by every pipeline test that omits `tools:` (e.g.
// TestRunPipeline_PassThrough): nothing is resolved because no step
// asks for anything. See pipeline_tools_resolve.go for the new path.
