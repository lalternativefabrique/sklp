package plan

import "testing"

// The YAML `cache:` field is a *bool so an omitted key is distinguishable from
// an explicit false. The compiler resolves that three-state value into one
// boolean the executor can read without re-deriving the default.
func TestCompileCI_CacheFieldResolution(t *testing.T) {
	tr, fa := true, false
	for _, tc := range []struct {
		name  string
		cache *bool
		want  bool // resulting CIStep.NoCache
	}{
		{"omitted defaults to cacheable", nil, false},
		{"cache: false opts out", &fa, true},
		{"cache: true is explicitly cacheable", &tr, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := CompileCI(CompileCIInput{
				Steps: []CIStepInput{{Name: "s", Run: "true", Cache: tc.cache}},
			}, allMatch, nil)
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			if got := out.CI.Steps[0].NoCache; got != tc.want {
				t.Fatalf("NoCache = %v, want %v", got, tc.want)
			}
		})
	}
}

// Opting one step out must not affect its neighbours — the flag is per step,
// not a plan-wide switch.
func TestCompileCI_CacheOptOutIsPerStep(t *testing.T) {
	fa := false
	out, err := CompileCI(CompileCIInput{
		Steps: []CIStepInput{
			{Name: "migrate", Run: "true", Cache: &fa},
			{Name: "test", Run: "true"},
		},
	}, allMatch, nil)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	byName := map[string]CIStep{}
	for _, s := range out.CI.Steps {
		byName[s.Name] = s
	}
	if !byName["migrate"].NoCache {
		t.Error("migrate: NoCache = false, want true")
	}
	if byName["test"].NoCache {
		t.Error("test: NoCache = true, want false (opt-out must not spread)")
	}
}
