package space

import "testing"

// The child user namespace is a decision the node's deploy launcher takes
// through UseChildUserns; nothing in the environment turns it on, so a dev or
// CI space built by the same binary stays the operator.
func TestChildUsernsIsNotAnEnvironmentSetting(t *testing.T) {
	t.Setenv("SKLP_SPACE_CHILD_USERNS", "1")
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	sp, err := NewSpace("policy")
	if err != nil {
		t.Skipf("NewSpace: %v", err)
	}
	if sp.childUserns {
		t.Fatal("NewSpace turned child userns on from the environment")
	}
	if err := sp.UseChildUserns(); err != nil || !sp.childUserns {
		t.Fatalf("UseChildUserns: %v", err)
	}
	standalone, err := NewSpace("")
	if err != nil {
		t.Skipf("NewSpace standalone: %v", err)
	}
	if err := standalone.UseChildUserns(); err == nil {
		t.Fatal("a standalone space must refuse a child userns")
	}
}
