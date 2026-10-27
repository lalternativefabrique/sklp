package controller

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/internal/xdg"
	"github.com/lalternativefabrique/sklp/spacemanager"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
)

// AddSpace's immediate-failure path (requireDurableVolumes, the cheapest way
// to fail runSpace before it ever touches a real runc launcher) must surface
// the error synchronously to the caller, without deadlocking on the ready
// channel, and must free the space name so a corrected retry is not rejected
// as a duplicate.
func TestAddSpace_ImmediateFailureSurfacesAndFreesName(t *testing.T) {
	t.Setenv(xdg.DataRootEnv, "")

	c := New()
	opts := SpaceOptions{
		Meta:     spacemanager.SpaceMeta{Name: "broken"},
		Services: []spacemanager.Service{{Name: "postgres", Volumes: []string{"pgdata:/var/lib/postgresql/data"}}},
	}

	err := c.AddSpace(context.Background(), opts)
	if err == nil {
		t.Fatal("expected requireDurableVolumes to fail AddSpace, got nil")
	}
	if !strings.Contains(err.Error(), "postgres") {
		t.Errorf("AddSpace error = %v, want it to mention the offending service", err)
	}

	c.mu.Lock()
	_, stillActive := c.active["broken"]
	c.mu.Unlock()
	if stillActive {
		t.Fatal("a failed space must not stay marked active, or a later retry is wrongly rejected as a duplicate")
	}
}

// A second AddSpace for a name already starting/running must be rejected
// immediately, without blocking — this is what lets the network dispatcher's
// singleflight-deduped materialize call be race-free even without its own
// lock: AddSpace itself refuses the duplicate.
func TestAddSpace_DuplicateNameRejected(t *testing.T) {
	c := New()
	c.mu.Lock()
	c.active["dup"] = struct{}{}
	c.mu.Unlock()

	err := c.AddSpace(context.Background(), SpaceOptions{Meta: spacemanager.SpaceMeta{Name: "dup"}})
	if err == nil {
		t.Fatal("expected AddSpace to reject an already-active space name, got nil")
	}
}

// A crash/early-failure (requireDurableVolumes here, same cheap trigger as
// TestAddSpace_ImmediateFailureSurfacesAndFreesName) must NOT purge the
// space's persisted manifest even with PurgeOnShutdown set — only a
// DELIBERATE Shutdown() call should ever trigger the purge. Losing a
// manifest to an unrelated failure would make a transient error
// indistinguishable from an explicit `sklp deploy delete`.
func TestAddSpace_PurgeOnShutdownDoesNotFireOnCrash(t *testing.T) {
	t.Setenv(xdg.DataRootEnv, "")
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	const space = "broken-purge"
	manifestPath := control.ManifestPath(space)
	if err := os.MkdirAll(manifestPath[:len(manifestPath)-len("/manifest.json")], 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := New()
	opts := SpaceOptions{
		Meta:            spacemanager.SpaceMeta{Name: space},
		Services:        []spacemanager.Service{{Name: "postgres", Volumes: []string{"pgdata:/var/lib/postgresql/data"}}},
		PurgeOnShutdown: true,
	}
	if err := c.AddSpace(context.Background(), opts); err == nil {
		t.Fatal("expected requireDurableVolumes to fail AddSpace, got nil")
	}

	if _, err := os.Stat(manifestPath); err != nil {
		t.Fatalf("manifest was removed on an unrelated failure (Shutdown was never called): %v", err)
	}
}

// OnStopped must fire whenever runSpace returns, for ANY reason — a crash
// (this test's trigger) included, not just a deliberate Shutdown(). A
// multi-space network dispatcher (control.Registry) relies on this to drop a
// dead space's Supervisor; without it, a subsequent apply for the same name
// reaches the STALE Supervisor instead of materializing fresh, and an
// identical manifest reports "unchanged" against a space that no longer
// exists on disk.
func TestAddSpace_OnStoppedFiresEvenOnCrash(t *testing.T) {
	t.Setenv(xdg.DataRootEnv, "")

	var stopped []string
	c := New()
	opts := SpaceOptions{
		Meta:      spacemanager.SpaceMeta{Name: "broken-onstopped"},
		Services:  []spacemanager.Service{{Name: "postgres", Volumes: []string{"pgdata:/var/lib/postgresql/data"}}},
		OnStopped: func(space string) { stopped = append(stopped, space) },
	}
	if err := c.AddSpace(context.Background(), opts); err == nil {
		t.Fatal("expected requireDurableVolumes to fail AddSpace, got nil")
	}

	if len(stopped) != 1 || stopped[0] != "broken-onstopped" {
		t.Fatalf("OnStopped calls = %v, want exactly one call with the space name", stopped)
	}
}

// An apply of a space whose deletion is still purging its dir must wait for
// that purge: started earlier, its new worker binds the attach socket that the
// purge then removes, and no container of the space can ever start.
func TestAddSpace_WaitsForTheSameNamesPurge(t *testing.T) {
	t.Setenv(xdg.DataRootEnv, "")
	c := New()
	purged := make(chan struct{})
	c.mu.Lock()
	c.purging = map[string]chan struct{}{"again": purged}
	c.mu.Unlock()

	done := make(chan error, 1)
	go func() {
		done <- c.AddSpace(context.Background(), SpaceOptions{
			Meta:     spacemanager.SpaceMeta{Name: "again"},
			Services: []spacemanager.Service{{Name: "postgres", Volumes: []string{"pgdata:/var/lib/postgresql/data"}}},
		})
	}()
	select {
	case <-done:
		t.Fatal("AddSpace ran while the same space was still purging")
	case <-time.After(100 * time.Millisecond):
	}
	c.mu.Lock()
	delete(c.purging, "again")
	c.mu.Unlock()
	close(purged)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("AddSpace never resumed after the purge finished")
	}
}

func TestAddSpace_PurgeWaitHonoursItsContext(t *testing.T) {
	c := New()
	c.mu.Lock()
	c.purging = map[string]chan struct{}{"stuck": make(chan struct{})}
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := c.AddSpace(ctx, SpaceOptions{Meta: spacemanager.SpaceMeta{Name: "stuck"}}); err == nil {
		t.Fatal("AddSpace returned nil while its context expired during the purge wait")
	}
}
