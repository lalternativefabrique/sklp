package space

import "testing"

// TestEnvKeyify pins parity with devstack.envKeyify / runner.envKeyify.
func TestEnvKeyify(t *testing.T) {
	for in, want := range map[string]string{
		"postgres-test": "POSTGRES_TEST",
		"nats":          "NATS",
		"a.b-c":         "A_B_C",
	} {
		if got := envKeyify(in); got != want {
			t.Errorf("envKeyify(%q) = %q, want %q", in, got, want)
		}
	}
}
