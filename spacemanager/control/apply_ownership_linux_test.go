package control

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/lalternativefabrique/sklp/spacemanager"
)

// seedManifestOwner writes a minimal persisted manifest for space, stamped
// with the given owning stack — the same shape applyDesired/persistManifest
// produce, just written directly so the test controls the "prior deploy"
// state without going through a real apply first.
func seedManifestOwner(t *testing.T, xdgConfig, space, stack string) {
	t.Helper()
	dir := filepath.Join(xdgConfig, "sklp", "spaces", space)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	spec := applySpec{
		Services: []spacemanager.Service{{Name: "placeholder"}},
		Meta:     spacemanager.SpaceMeta{Name: space, Stack: stack},
	}
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestHandleApply_CrossStackRefusedWithoutForce is the regression guard for
// the bug this file fixes: a remote `sklp deploy` reconciling a space already
// owned by a DIFFERENT stack must be refused, matching the local sklet apply
// guard (TryReapply) — not silently adopt the space and reconcile away its
// services.
func TestHandleApply_CrossStackRefusedWithoutForce(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	seedManifestOwner(t, xdg, "production", "www")

	sup := &fakeSupervisor{
		ready:   true,
		differs: map[string]bool{},
		live:    map[string]spacemanager.Service{},
	}
	srv, fp := newTestNetServer(t, sup)
	c, err := DialNet(srv.URL, testToken, WithCAFingerprint(fp))
	if err != nil {
		t.Fatalf("DialNet: %v", err)
	}
	c = c.WithSpace("production")

	spec, err := MarshalApplySpec([]spacemanager.Service{{Name: "nats"}}, spacemanager.SpaceMeta{Name: "production", Stack: "nats-global"})
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}

	if _, err := c.Apply(context.Background(), spec); err == nil {
		t.Fatal("expected the cross-stack apply to be refused, got nil error")
	}
	if len(sup.added) != 0 {
		t.Fatalf("supervisor was mutated despite the refusal: %v", sup.added)
	}
}

// TestHandleApply_CrossStackAdoptedWithForce proves --force (ApplyForce) is
// the working escape hatch: the same cross-stack apply that TestHandleApply_
// CrossStackRefusedWithoutForce refuses must succeed and reach the supervisor
// when Force is set.
func TestHandleApply_CrossStackAdoptedWithForce(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	seedManifestOwner(t, xdg, "production", "www")

	sup := &fakeSupervisor{
		ready:   true,
		differs: map[string]bool{},
		live:    map[string]spacemanager.Service{},
	}
	srv, fp := newTestNetServer(t, sup)
	c, err := DialNet(srv.URL, testToken, WithCAFingerprint(fp))
	if err != nil {
		t.Fatalf("DialNet: %v", err)
	}
	c = c.WithSpace("production")

	spec, err := MarshalApplySpec([]spacemanager.Service{{Name: "nats"}}, spacemanager.SpaceMeta{Name: "production", Stack: "nats-global"})
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}

	if _, err := c.ApplyForce(context.Background(), spec); err != nil {
		t.Fatalf("ApplyForce: %v", err)
	}
	if len(sup.added) != 1 || sup.added[0] != "nats" {
		t.Fatalf("addedServices = %v, want [nats]", sup.added)
	}
}

// TestHandleApply_SameStackNeedsNoForce proves the guard only fires on a
// STACK MISMATCH: a re-apply from the SAME stack that already owns the space
// (the common case — a redeploy) must succeed without --force, exactly as
// before this fix.
func TestHandleApply_SameStackNeedsNoForce(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	seedManifestOwner(t, xdg, "production", "www")

	sup := &fakeSupervisor{
		ready:   true,
		differs: map[string]bool{},
		live:    map[string]spacemanager.Service{},
	}
	srv, fp := newTestNetServer(t, sup)
	c, err := DialNet(srv.URL, testToken, WithCAFingerprint(fp))
	if err != nil {
		t.Fatalf("DialNet: %v", err)
	}
	c = c.WithSpace("production")

	spec, err := MarshalApplySpec([]spacemanager.Service{{Name: "www"}}, spacemanager.SpaceMeta{Name: "production", Stack: "www"})
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}

	if _, err := c.Apply(context.Background(), spec); err != nil {
		t.Fatalf("same-stack Apply should not require --force: %v", err)
	}
}

// TestHandleApply_NoPriorManifestNeedsNoForce proves a first deploy (no
// persisted manifest yet) is never blocked — the guard is "unknown owner
// never refuses", matching the local TryReapply behaviour.
func TestHandleApply_NoPriorManifestNeedsNoForce(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg) // no seeded manifest at all

	sup := &fakeSupervisor{
		ready:   true,
		differs: map[string]bool{},
		live:    map[string]spacemanager.Service{},
	}
	srv, fp := newTestNetServer(t, sup)
	c, err := DialNet(srv.URL, testToken, WithCAFingerprint(fp))
	if err != nil {
		t.Fatalf("DialNet: %v", err)
	}
	c = c.WithSpace("production")

	spec, err := MarshalApplySpec([]spacemanager.Service{{Name: "www"}}, spacemanager.SpaceMeta{Name: "production", Stack: "www"})
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}

	if _, err := c.Apply(context.Background(), spec); err != nil {
		t.Fatalf("first deploy (no prior manifest) should not require --force: %v", err)
	}
}
