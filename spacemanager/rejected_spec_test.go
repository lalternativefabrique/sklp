package spacemanager

import "testing"

// The apply handler asks the agent, not the manifest on disk, which spec a
// service converges to and whether an incoming one is the spec just rolled
// back; the on-disk manifest can lag behind a rollback, the agent cannot.
func TestDesiredServiceAndRejectedSpec(t *testing.T) {
	desired := []Service{{Name: "web", Image: "rg/web:1", DeclaredImage: "rg/web:1", Command: []string{"true"}}}
	a := NewAgent(newExecLauncher(), nil, desired, testOpts())

	if _, ok := a.DesiredService("ghost"); ok {
		t.Fatal("an unknown service reported a desired spec")
	}
	if got, ok := a.DesiredService("web"); !ok || got.Image != "rg/web:1" {
		t.Fatalf("desired web = %+v (%v), want the steady-state spec", got, ok)
	}

	broken := Service{Name: "web", Image: "rg/web:2", DeclaredImage: "rg/web:1", Command: []string{"true"}}
	if _, rejected := a.RejectedSpec(broken); rejected {
		t.Fatal("a spec never rolled back reported as rejected")
	}

	a.mu.Lock()
	st := a.services["web"]
	st.rejected = &broken
	st.rollouts = append(st.rollouts, RolloutRecord{Result: RolloutRolledBack, Reason: "exited 1 before ready"})
	a.mu.Unlock()

	reason, rejected := a.RejectedSpec(broken)
	if !rejected || reason != "exited 1 before ready" {
		t.Fatalf("RejectedSpec = (%q, %v), want the rollback reason", reason, rejected)
	}
	if _, rejected := a.RejectedSpec(Service{Name: "web", Image: "rg/web:3", Command: []string{"true"}}); rejected {
		t.Fatal("a different spec reported as rejected")
	}
	if got, _ := a.DesiredService("web"); got.Image != "rg/web:1" {
		t.Fatalf("desired web after rollback = %q, want the running rg/web:1", got.Image)
	}
}
