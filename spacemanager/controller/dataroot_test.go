package controller

import (
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/internal/xdg"
	"github.com/lalternativefabrique/sklp/spacemanager"
)

// The whole point: a node that stores nothing durably must refuse a space whose
// services declare named volumes, instead of dropping a database into the
// runtime cache where `sklp clean` will eventually reach it.
func TestRequireDurableVolumes_RefusesNamedVolumesWithoutDataRoot(t *testing.T) {
	t.Setenv(xdg.DataRootEnv, "")

	err := requireDurableVolumes([]spacemanager.Service{
		{Name: "postgres", Volumes: []string{"pgdata:/var/lib/postgresql/data"}},
	})
	if err == nil {
		t.Fatal("a named volume was accepted on a node with no data root")
	}
	for _, want := range []string{"postgres", "pgdata", xdg.DataRootEnv} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q, so the operator cannot act on it: %v", want, err)
		}
	}
}

// With a data root the same space is fine — volumes land on the disk.
func TestRequireDurableVolumes_AcceptsWithDataRoot(t *testing.T) {
	t.Setenv(xdg.DataRootEnv, t.TempDir())

	if err := requireDurableVolumes([]spacemanager.Service{
		{Name: "postgres", Volumes: []string{"pgdata:/var/lib/postgresql/data"}},
	}); err != nil {
		t.Errorf("a named volume was refused despite a data root: %v", err)
	}
}

// A space with no named volumes has nothing to lose, so it must keep deploying
// on a node with no data disk — otherwise the check breaks stateless apps.
func TestRequireDurableVolumes_AllowsStatelessSpace(t *testing.T) {
	t.Setenv(xdg.DataRootEnv, "")

	if err := requireDurableVolumes([]spacemanager.Service{
		{Name: "web"},
		{Name: "api", Volumes: nil},
	}); err != nil {
		t.Errorf("a space with no volumes was refused: %v", err)
	}
}

// A host bind names a durable location the operator already chose, so it is not
// sklp's to refuse — only named volumes are materialised by sklp.
func TestRequireDurableVolumes_AllowsHostBinds(t *testing.T) {
	t.Setenv(xdg.DataRootEnv, "")

	if err := requireDurableVolumes([]spacemanager.Service{
		{Name: "app", Volumes: []string{"/srv/app/data:/data"}},
	}); err != nil {
		t.Errorf("a host bind was refused: %v", err)
	}
}
