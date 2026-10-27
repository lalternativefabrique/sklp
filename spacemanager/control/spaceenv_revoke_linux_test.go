//go:build linux

package control

import (
	"testing"
)

// A var removed from the store used to stay on the node forever: MergeSpaceEnv
// unions, so nothing ever dropped a key. The service kept receiving a secret
// its manifest no longer declared, and revoking one meant editing the node by
// hand.
func TestPersistSpaceEnvDropsAVarTheApplyNoLongerResolves(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const space = "production"
	if err := WriteSpaceEnv(space, map[string]string{"KEEP": "v1", "GONE": "v2"}); err != nil {
		t.Fatalf("WriteSpaceEnv: %v", err)
	}

	// A remote apply that resolves only KEEP: its list is authoritative.
	if err := persistSpaceEnv(space, map[string]string{"KEEP": "v1"}, true); err != nil {
		t.Fatalf("persistSpaceEnv: %v", err)
	}

	vars, err := ReadSpaceEnv(space)
	if err != nil {
		t.Fatalf("ReadSpaceEnv: %v", err)
	}
	if _, still := vars["GONE"]; still {
		t.Error("GONE survived an apply that no longer resolves it; revoking a secret has no effect")
	}
	if vars["KEEP"] != "v1" {
		t.Errorf("KEEP = %q, want it kept", vars["KEEP"])
	}
}

// An image roll rebuilds the manifest from disk and resolves nothing, so its
// map is not a statement about what the space needs. Replacing on it would
// wipe every secret the space has — the reason the union existed.
func TestPersistSpaceEnvKeepsEverythingWhenTheApplyResolvedNothing(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const space = "production"
	if err := WriteSpaceEnv(space, map[string]string{"KEEP": "v1", "ALSO": "v2"}); err != nil {
		t.Fatalf("WriteSpaceEnv: %v", err)
	}

	if err := persistSpaceEnv(space, map[string]string{"KEEP": "v1"}, false); err != nil {
		t.Fatalf("persistSpaceEnv: %v", err)
	}

	vars, err := ReadSpaceEnv(space)
	if err != nil {
		t.Fatalf("ReadSpaceEnv: %v", err)
	}
	if vars["ALSO"] != "v2" {
		t.Error("an image roll dropped a secret it never claimed to know about")
	}
}

// A var referenced only inside a composed value ("https://${HOST}") lives
// nowhere else on the node, so it has to survive as long as the apply that
// resolved it still names it.
func TestPersistSpaceEnvKeepsAVarUsedOnlyInsideATemplate(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const space = "production"
	if err := WriteSpaceEnv(space, map[string]string{"HOST": "example.com"}); err != nil {
		t.Fatalf("WriteSpaceEnv: %v", err)
	}

	if err := persistSpaceEnv(space, map[string]string{"HOST": "example.com", "TOKEN": "t1"}, true); err != nil {
		t.Fatalf("persistSpaceEnv: %v", err)
	}

	vars, err := ReadSpaceEnv(space)
	if err != nil {
		t.Fatalf("ReadSpaceEnv: %v", err)
	}
	if vars["HOST"] != "example.com" {
		t.Error("HOST was dropped though the apply still resolves it")
	}
	if vars["TOKEN"] != "t1" {
		t.Error("TOKEN was not persisted")
	}
}

// The last secret of a space going away must leave no file behind for the unit
// to keep sourcing.
func TestPersistSpaceEnvRemovesTheFileWhenNothingIsLeft(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const space = "production"
	if err := WriteSpaceEnv(space, map[string]string{"GONE": "v1"}); err != nil {
		t.Fatalf("WriteSpaceEnv: %v", err)
	}

	if err := persistSpaceEnv(space, map[string]string{}, true); err != nil {
		t.Fatalf("persistSpaceEnv: %v", err)
	}

	vars, err := ReadSpaceEnv(space)
	if err != nil {
		t.Fatalf("ReadSpaceEnv: %v", err)
	}
	if len(vars) != 0 {
		t.Errorf("vars = %v, want none", vars)
	}
}
