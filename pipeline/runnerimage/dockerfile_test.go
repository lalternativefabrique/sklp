package runnerimage

import (
	"testing"

	"github.com/lalternativefabrique/sklp/space/host"
)

// TestRunnerBuildPlan_Beta folds the real acme recipe into a plan and
// asserts the shape: the base, the apt + skt + chmod RUNs, and the ENV/WORKDIR
// metadata carried separately (not as RUN steps).
func TestRunnerBuildPlan_Beta(t *testing.T) {
	r := baseRunner() // go+node, 2 packages, 1 go_tool, GOPRIVATE env
	plan, err := RunnerBuildPlan(r)
	if err != nil {
		t.Fatalf("RunnerBuildPlan: %v", err)
	}
	if plan.BaseImage != r.BaseImage {
		t.Errorf("BaseImage = %q, want %q", plan.BaseImage, r.BaseImage)
	}
	// RUNs: 1 apt + 1 mkdir + 2 toolchain + 1 go_tool + 1 chmod = 6.
	if len(plan.Runs) != 6 {
		t.Errorf("got %d RUN steps, want 6:\n%v", len(plan.Runs), plan.Runs)
	}
	if plan.Runs[0] == "" || !contains(plan.Runs[0], "apt-get install") {
		t.Errorf("first RUN should be the apt install, got %q", plan.Runs[0])
	}
	// ENV metadata, not RUN. SKLP_TOOLS_CACHE/PATH/GOTOOLCHAIN + GOPRIVATE.
	for _, k := range []string{"SKLP_TOOLS_CACHE", "PATH", "GOTOOLCHAIN", "GOPRIVATE"} {
		if _, ok := plan.Env[k]; !ok {
			t.Errorf("ENV %s missing from plan metadata", k)
		}
	}
	if plan.Workdir != "/workspace" {
		t.Errorf("Workdir = %q, want /workspace", plan.Workdir)
	}
}

// TestRunnerBuildPlan_NoPackages: a recipe without packages still installs the
// host packages every Space needs (nft, uidmap, util-linux…) and nothing else.
func TestRunnerBuildPlan_NoPackages(t *testing.T) {
	r := baseRunner()
	r.Packages = nil
	plan, err := RunnerBuildPlan(r)
	if err != nil {
		t.Fatalf("RunnerBuildPlan: %v", err)
	}
	var apt string
	for _, run := range plan.Runs {
		if contains(run, "apt-get") {
			apt = run
		}
	}
	if apt == "" {
		t.Fatal("host packages must be installed even when the recipe declares none")
	}
	for _, want := range host.Packages {
		if !contains(apt, " "+want+" ") && !contains(apt, " "+want+" &&") {
			t.Errorf("host package %s missing from %q", want, apt)
		}
	}
}

// TestParseRunnerDockerfile_RejectsCopy: a COPY (multi-stage / app Dockerfile)
// must be rejected — the Go builder only handles the generated runner grammar.
func TestParseRunnerDockerfile_RejectsCopy(t *testing.T) {
	df := "FROM base\nCOPY . /app\n"
	if _, err := ParseRunnerDockerfile(df); err == nil {
		t.Fatal("expected COPY to be rejected")
	}
}

// TestParseRunnerDockerfile_RejectsMultiStage: a second FROM is rejected at the
// plan stage.
func TestParseRunnerDockerfile_RejectsMultiStage(t *testing.T) {
	df := "FROM a\nRUN x\nFROM b\n"
	ins, err := ParseRunnerDockerfile(df)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := PlanFromInstructions(ins); err == nil {
		t.Fatal("expected multi-stage FROM to be rejected")
	}
}

// TestParseRunnerDockerfile_RequiresFromFirst.
func TestParseRunnerDockerfile_RequiresFromFirst(t *testing.T) {
	if _, err := ParseRunnerDockerfile("RUN x\n"); err == nil {
		t.Fatal("expected error when Dockerfile does not start with FROM")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
