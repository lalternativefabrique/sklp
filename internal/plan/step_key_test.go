package plan

import "testing"

// makeStep builds a CIStep with sensible defaults for key tests.
func makeStep(name, run string, env map[string]string, tools []string) CIStep {
	return CIStep{
		Name:   name,
		Run:    run,
		Env:    env,
		Tools:  tools,
		Status: StatusIncluded,
	}
}

func TestComputeCIStepKey_StableAcrossCalls(t *testing.T) {
	step := makeStep("lint", "pnpm lint", map[string]string{"FOO": "1", "BAR": "2"}, []string{"pnpm"})
	pins := map[string]string{"pnpm": "10.18.1"}

	k1, err := ComputeCIStepKey(step, pins, "upstream", "")
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	k2, err := ComputeCIStepKey(step, pins, "upstream", "")
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if k1 != k2 {
		t.Fatalf("key changed across identical calls: %s vs %s", k1, k2)
	}
}

func TestComputeCIStepKey_ChangesWithCommand(t *testing.T) {
	pins := map[string]string{}
	k1, _ := ComputeCIStepKey(makeStep("s", "echo a", nil, nil), pins, "", "")
	k2, _ := ComputeCIStepKey(makeStep("s", "echo b", nil, nil), pins, "", "")
	if k1 == k2 {
		t.Fatalf("changing Run did not invalidate key")
	}
}

func TestComputeCIStepKey_ChangesWithEnv(t *testing.T) {
	pins := map[string]string{}
	k1, _ := ComputeCIStepKey(makeStep("s", "echo", map[string]string{"X": "1"}, nil), pins, "", "")
	k2, _ := ComputeCIStepKey(makeStep("s", "echo", map[string]string{"X": "2"}, nil), pins, "", "")
	if k1 == k2 {
		t.Fatalf("changing Env value did not invalidate key")
	}
}

func TestComputeCIStepKey_ChangesWithToolVersion(t *testing.T) {
	step := makeStep("s", "echo", nil, []string{"pnpm"})
	k1, _ := ComputeCIStepKey(step, map[string]string{"pnpm": "10.18.1"}, "", "")
	k2, _ := ComputeCIStepKey(step, map[string]string{"pnpm": "10.19.0"}, "", "")
	if k1 == k2 {
		t.Fatalf("changing pinned tool version did not invalidate key")
	}
}

func TestComputeCIStepKey_ChangesWithUpstream(t *testing.T) {
	step := makeStep("s", "echo", nil, nil)
	k1, _ := ComputeCIStepKey(step, nil, "alpha", "")
	k2, _ := ComputeCIStepKey(step, nil, "beta", "")
	if k1 == k2 {
		t.Fatalf("changing upstream did not invalidate key (downstream invalidation broken)")
	}
}

func TestComputeCIStepKey_ChangesWithNetwork(t *testing.T) {
	pins := map[string]string{}
	s1 := makeStep("s", "echo", nil, nil)
	s2 := makeStep("s", "echo", nil, nil)
	s2.Network = "none"
	k1, _ := ComputeCIStepKey(s1, pins, "", "")
	k2, _ := ComputeCIStepKey(s2, pins, "", "")
	if k1 == k2 {
		t.Fatalf("changing Network did not invalidate key")
	}
}

func TestComputeCIStepKey_ChangesWithSecrets(t *testing.T) {
	pins := map[string]string{}
	s1 := makeStep("s", "echo", nil, nil)
	s1.Secrets = []string{"A"}
	s2 := makeStep("s", "echo", nil, nil)
	s2.Secrets = []string{"A", "B"}
	k1, _ := ComputeCIStepKey(s1, pins, "", "")
	k2, _ := ComputeCIStepKey(s2, pins, "", "")
	if k1 == k2 {
		t.Fatalf("adding a secret did not invalidate key")
	}
}

func TestComputeCIStepKey_SecretsOrderIrrelevant(t *testing.T) {
	pins := map[string]string{}
	s1 := makeStep("s", "echo", nil, nil)
	s1.Secrets = []string{"A", "B", "C"}
	s2 := makeStep("s", "echo", nil, nil)
	s2.Secrets = []string{"C", "A", "B"}
	k1, _ := ComputeCIStepKey(s1, pins, "", "")
	k2, _ := ComputeCIStepKey(s2, pins, "", "")
	if k1 != k2 {
		t.Fatalf("reordering Secrets changed key: %s vs %s", k1, k2)
	}
}

func TestComputeCIStepKey_ChangesWithDependsOn(t *testing.T) {
	pins := map[string]string{}
	s1 := makeStep("s", "echo", nil, nil)
	s1.DependsOn = []string{"a"}
	s2 := makeStep("s", "echo", nil, nil)
	s2.DependsOn = []string{"a", "b"}
	k1, _ := ComputeCIStepKey(s1, pins, "", "")
	k2, _ := ComputeCIStepKey(s2, pins, "", "")
	if k1 == k2 {
		t.Fatalf("changing DependsOn did not invalidate key")
	}
}

func TestComputeCIStepKey_DependsOnOrderIrrelevant(t *testing.T) {
	pins := map[string]string{}
	s1 := makeStep("s", "echo", nil, nil)
	s1.DependsOn = []string{"a", "b", "c"}
	s2 := makeStep("s", "echo", nil, nil)
	s2.DependsOn = []string{"c", "b", "a"}
	k1, _ := ComputeCIStepKey(s1, pins, "", "")
	k2, _ := ComputeCIStepKey(s2, pins, "", "")
	if k1 != k2 {
		t.Fatalf("reordering DependsOn changed key: %s vs %s", k1, k2)
	}
}

func TestComputeCIPlanKey_StableButSensitive(t *testing.T) {
	plan := &Plan{
		SchemaVersion: SchemaVersion,
		Kind:          KindCI,
		CI: &CIPlan{
			Steps: []CIStep{
				makeStep("a", "echo a", nil, nil),
				makeStep("b", "echo b", nil, nil),
			},
		},
	}
	k1, err := ComputeCIPlanKey(plan, "")
	if err != nil {
		t.Fatalf("plan key: %v", err)
	}
	k2, err := ComputeCIPlanKey(plan, "")
	if err != nil {
		t.Fatalf("plan key: %v", err)
	}
	if k1 != k2 {
		t.Fatalf("plan key changed across identical calls")
	}

	plan.CI.Steps[0].Run = "echo modified"
	k3, _ := ComputeCIPlanKey(plan, "")
	if k3 == k1 {
		t.Fatalf("plan key did not change after step Run mutation")
	}
}

func TestComputeCIStepKey_ChangesWithPlatform(t *testing.T) {
	publish := func(platform string) CIStep {
		s := makeStep("img", "", nil, nil)
		s.StepKind = KindCIStepPublish
		s.Publish = &PublishSpec{Image: "r/app", Tag: "v1", Context: ".", Dockerfile: "Dockerfile", Platform: platform}
		return s
	}
	k1, err := ComputeCIStepKey(publish("linux/amd64"), nil, "", "")
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	k2, _ := ComputeCIStepKey(publish("linux/arm64"), nil, "", "")
	if k1 == k2 {
		t.Fatal("key did not change with platform")
	}
}
