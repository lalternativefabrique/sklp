package pipeline

import "testing"

// The Space is the only execution backend. alreadyInSandbox decides whether
// the run executes directly in-process (we are already inside the Space/Pod)
// or spawns a Space. SKLP_RUNNER_MODE is the sole signal.
func TestAlreadyInSandbox(t *testing.T) {
	cases := []struct {
		name string
		mode string
		want bool
	}{
		{"unset → spawn space", "", false},
		{"embedded → direct", RunnerModeEmbedded, true},
		{"k8s → direct", RunnerModeK8s, true},
		{"unknown → spawn space", "other", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(RunnerModeEnv, tc.mode)
			if got := alreadyInSandbox(); got != tc.want {
				t.Errorf("alreadyInSandbox() = %v, want %v", got, tc.want)
			}
		})
	}
}
