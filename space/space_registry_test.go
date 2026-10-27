package space

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/space/spaceapi"
)

// newTestRegistry builds a registry over a temp cache with a fake runc that
// answers list/state from the per-space runc-state dir (deployps_test fake).
func newTestRegistry(t *testing.T, listSupervised func() ([]string, error)) (*runcRegistry, string) {
	t.Helper()
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("SKLP_RUNC_PATH", fakeRuncForPs(t))
	t.Setenv("SKLP_UNSAFE_RUNC_PATH", "1")
	reg, err := NewRegistry(listSupervised)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	root := filepath.Join(cache, "sklp", "embedded-runtime")
	return reg.(*runcRegistry), root
}

// seedSpace materialises a space's per-space dirs and (optionally) a running
// container in its runc state, so the registry's scan + ListDeployContainers see
// it the same way a real deploy would.
func seedSpace(t *testing.T, root, name string, containers []string, volumes []string) {
	t.Helper()
	scope := newScope(name, root)
	state := scope.runcStateRoot()
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, id := range containers {
		if err := os.WriteFile(filepath.Join(state, id), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, v := range volumes {
		if err := os.MkdirAll(filepath.Join(scope.volumesRoot(), v), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRegistryList(t *testing.T) {
	reg, root := newTestRegistry(t, func() ([]string, error) { return []string{"web"}, nil })
	// "web" has a running container + a volume; "idle" keeps a volume but runs nothing.
	seedSpace(t, root, "web", []string{"sklp-deploy-web-api-0"}, []string{"pgdata"})
	seedSpace(t, root, "idle", nil, []string{"data"})

	infos, err := reg.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(infos) != 2 {
		t.Fatalf("listed %d spaces, want 2: %+v", len(infos), infos)
	}
	// Sorted by name: idle, web.
	if infos[0].Name != "idle" || infos[1].Name != "web" {
		t.Fatalf("unsorted: %s, %s", infos[0].Name, infos[1].Name)
	}
	web := infos[1]
	if web.Status != "running" {
		t.Fatalf("web status = %q, want running", web.Status)
	}
	if len(web.Containers) != 1 || web.Containers[0].ID != "sklp-deploy-web-api-0" {
		t.Fatalf("web containers = %+v", web.Containers)
	}
	if len(web.Volumes) != 1 || web.Volumes[0] != "pgdata" {
		t.Fatalf("web volumes = %+v", web.Volumes)
	}
	if !web.Supervised {
		t.Fatalf("web should be flagged supervised (live socket)")
	}
	if infos[0].Status != "" {
		t.Fatalf("idle status = %q, want empty (no containers)", infos[0].Status)
	}
}

func TestRegistryGetNotFound(t *testing.T) {
	reg, _ := newTestRegistry(t, nil)
	_, err := reg.Get(context.Background(), "ghost")
	if err != spaceapi.ErrSpaceNotFound {
		t.Fatalf("Get(ghost) err = %v, want ErrSpaceNotFound", err)
	}
}

func TestRegistryGet(t *testing.T) {
	reg, root := newTestRegistry(t, nil)
	seedSpace(t, root, "db", []string{"sklp-deploy-db-pg-0"}, []string{"data"})
	info, err := reg.Get(context.Background(), "db")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if info.Name != "db" || info.Status != "running" {
		t.Fatalf("got %+v", info)
	}
	if info.Supervised {
		t.Fatalf("nil lister must leave Supervised false")
	}
}

func TestRegistryUpExists(t *testing.T) {
	reg, root := newTestRegistry(t, nil)
	seedSpace(t, root, "taken", nil, nil)
	err := reg.Up(context.Background(), "taken", spaceapi.UpSpec{})
	if err != spaceapi.ErrSpaceExists {
		t.Fatalf("Up on existing space err = %v, want ErrSpaceExists", err)
	}
}

// Deleting a space deletes the space: nothing of it survives on disk, volumes
// included. Holding the volumes back kept the space dir alive, and since List
// enumerates those dirs the space reappeared on the next refresh with no command
// left able to remove it.
func TestRegistryDownRemovesEverything(t *testing.T) {
	reg, root := newTestRegistry(t, nil)
	seedSpace(t, root, "gone", []string{"sklp-deploy-gone-x-0"}, []string{"vol"})
	scope := newScope("gone", root)

	if err := reg.Down(context.Background(), "gone"); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if _, err := os.Stat(scope.spaceDir()); !os.IsNotExist(err) {
		t.Fatalf("space dir survived down: %v", err)
	}
}

// The space must not come back through List once torn down — the symptom that
// exposed the kept-volumes behaviour in the first place.
func TestRegistryDownLeavesNothingToList(t *testing.T) {
	reg, root := newTestRegistry(t, nil)
	seedSpace(t, root, "keep", []string{"sklp-deploy-keep-x-0"}, []string{"vol"})

	if err := reg.Down(context.Background(), "keep"); err != nil {
		t.Fatalf("Down: %v", err)
	}
	infos, err := reg.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, info := range infos {
		if info.Name == "keep" {
			t.Fatal("a torn-down space must not reappear in List")
		}
	}
}

// A space whose state dir is gone is NOT necessarily a space with nothing
// running: the dir is sklp's only handle, so a leaked service survives it
// invisibly. Down must still drain that space's cgroups, or the leak has no
// sklp-native remedy and every later run fails on "cgroup is not empty".
func TestRegistryDownReapsCgroupsOfVanishedSpace(t *testing.T) {
	reg, _ := newTestRegistry(t, nil)

	var reaped []string
	prev := reapSpaceCgroups
	reapSpaceCgroups = func(prefix string) { reaped = append(reaped, prefix) }
	t.Cleanup(func() { reapSpaceCgroups = prev })

	if err := reg.Down(context.Background(), "vanished"); err != nil {
		t.Fatalf("Down on a missing space: %v", err)
	}
	want := newScope("vanished", "").containerIDPrefix()
	if len(reaped) != 1 || reaped[0] != want {
		t.Fatalf("reaped prefixes = %v, want exactly [%s]", reaped, want)
	}
}

func TestRegistryPurge(t *testing.T) {
	reg, root := newTestRegistry(t, nil)
	// "live" has a running container (kept); "stale" has only leftover state
	// (no live container in the fake — empty state dir reads as stopped).
	seedSpace(t, root, "live", []string{"sklp-deploy-live-x-0"}, nil)
	seedSpace(t, root, "stale", nil, nil)

	reaped, err := reg.Purge(context.Background())
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if len(reaped) != 1 || reaped[0] != "stale" {
		t.Fatalf("purged %v, want [stale]", reaped)
	}
	if !reg.exists("live") {
		t.Fatalf("live space was purged but has a running container")
	}
	if reg.exists("stale") {
		t.Fatalf("stale space survived purge")
	}
}

// TestRegistryPurge_ContinueOnError asserts one stuck space (a subuid-owned
// volume the operator cannot delete even via userns) does not freeze the GC:
// the remaining spaces are still reaped and the failure is returned joined.
func TestRegistryPurge_ContinueOnError(t *testing.T) {
	// Stub the removal seam so "aaa" always fails (independent of host newuidmap);
	// every other space removes normally.
	orig := subidRemoveAll
	t.Cleanup(func() { subidRemoveAll = orig })
	subidRemoveAll = func(path string) error {
		if strings.Contains(path, string(os.PathSeparator)+"aaa") {
			return fmt.Errorf("permission denied")
		}
		return orig(path)
	}

	reg, root := newTestRegistry(t, nil)
	// "aaa" sorts first and is wedged; "zzz" sorts last and must still be reaped.
	seedSpace(t, root, "aaa", nil, []string{"vol"})
	seedSpace(t, root, "zzz", nil, []string{"vol"})

	reaped, err := reg.Purge(context.Background())
	if err == nil {
		t.Fatal("Purge returned nil error despite a wedged space")
	}
	if !strings.Contains(err.Error(), "aaa") {
		t.Fatalf("joined error must name the failing space, got: %v", err)
	}
	if len(reaped) != 1 || reaped[0] != "zzz" {
		t.Fatalf("reaped %v, want [zzz] (wedged aaa must not block it)", reaped)
	}
	if !reg.exists("aaa") {
		t.Fatal("aaa should survive: its volume removal failed")
	}
	if reg.exists("zzz") {
		t.Fatal("zzz survived purge but had no running container")
	}
}

// Stop is the non-destructive half of Down: the containers go, the data stays.
// Down deletes the volumes with the space, which is why a Postgres dev DB could
// not survive a teardown from the cockpit before this existed.
func TestRegistryStopKeepsVolumes(t *testing.T) {
	reg, root := newTestRegistry(t, nil)
	seedSpace(t, root, "keepdata", []string{"sklp-deploy-keepdata-x-0"}, []string{"pgdata"})
	scope := newScope("keepdata", root)
	volume := filepath.Join(scope.volumesRoot(), "pgdata")
	payload := filepath.Join(volume, "PG_VERSION")
	if err := os.WriteFile(payload, []byte("17"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := reg.Stop(context.Background(), "keepdata"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := os.Stat(scope.spaceDir()); err != nil {
		t.Fatalf("the space dir must survive a stop: %v", err)
	}
	got, err := os.ReadFile(payload)
	if err != nil {
		t.Fatalf("volume data must survive a stop: %v", err)
	}
	if string(got) != "17" {
		t.Errorf("volume content changed: %q", got)
	}
}

// A stopped space stays listed — otherwise there would be nothing left to `up`.
func TestRegistryStopKeepsTheSpaceListed(t *testing.T) {
	reg, root := newTestRegistry(t, nil)
	seedSpace(t, root, "halted", []string{"sklp-deploy-halted-x-0"}, []string{"vol"})

	if err := reg.Stop(context.Background(), "halted"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	infos, err := reg.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, info := range infos {
		if info.Name == "halted" {
			return
		}
	}
	t.Fatal("a stopped space must still be listed, or nothing can bring it back up")
}

// Down is an idempotent teardown, but stopping a space that does not exist is a
// mistake worth reporting rather than a silent success.
func TestRegistryStopUnknownSpace(t *testing.T) {
	reg, _ := newTestRegistry(t, nil)
	if err := reg.Stop(context.Background(), "nope"); !errors.Is(err, spaceapi.ErrSpaceNotFound) {
		t.Fatalf("Stop on a missing space = %v, want ErrSpaceNotFound", err)
	}
}

// A bare spaces/<name>/runc-state dir (what an agent's first boot leaves for
// its identity space) is not a space: nothing runs, nothing is kept, nothing
// can be brought up. Listing it shows an empty space no delete removes.
func TestRegistryListSkipsEmptyShell(t *testing.T) {
	reg, root := newTestRegistry(t, nil)
	seedSpace(t, root, "production", nil, nil)
	seedSpace(t, root, "kept", nil, []string{"pg"})

	infos, err := reg.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var names []string
	for _, info := range infos {
		names = append(names, info.Name)
	}
	if len(names) != 1 || names[0] != "kept" {
		t.Fatalf("listed = %v, want only the space holding a volume", names)
	}
}
