package control

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/lalternativefabrique/sklp/spacemanager"
)

func TestApplyDesired_RawSpecGetsDefaultPidsLimit(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	sup := &fakeSupervisor{ready: true, differs: map[string]bool{}, live: map[string]spacemanager.Service{}}

	if _, err := applyDesired(sup, []spacemanager.Service{{Name: "web", Image: "nginx"}}, spacemanager.SpaceMeta{Name: "production"}); err != nil {
		t.Fatalf("applyDesired: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(xdg, "sklp", "spaces", "production", "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var got applySpec
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Services) != 1 || got.Services[0].Resources.PidsMax == 0 {
		t.Fatalf("raw-spec service persisted without a pids limit: %+v", got.Services)
	}
}
