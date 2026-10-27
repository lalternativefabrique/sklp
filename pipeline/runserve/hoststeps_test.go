package runserve

import (
	"os"
	"path/filepath"
	"testing"
)

func writePipeline(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ci.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCheckHostStepsRefusesHostPipelineByDefault(t *testing.T) {
	host := writePipeline(t, "name: ci\nspace: false\nsteps:\n  - name: a\n    run: echo hi\n")
	if err := (&Server{}).checkHostSteps(host); err == nil {
		t.Fatal("space: false pipeline accepted without --allow-host-steps")
	}
	if err := (&Server{AllowHostSteps: true}).checkHostSteps(host); err != nil {
		t.Fatalf("operator opt-in refused: %v", err)
	}
}

func TestCheckHostStepsAcceptsSandboxedPipeline(t *testing.T) {
	sandboxed := writePipeline(t, "name: ci\nsteps:\n  - name: a\n    run: echo hi\n")
	if err := (&Server{}).checkHostSteps(sandboxed); err != nil {
		t.Fatalf("sandboxed pipeline refused: %v", err)
	}
}
