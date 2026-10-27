package spacemanager

import (
	"slices"
	"testing"
)

// NamedSpaces lists every materialisable space (sorted) and EXCLUDES the
// ephemeral mould — `sklp deploy --all` iterates it, and bringing up the mould
// itself (a template, not a space) would be a bug.
func TestNamedSpaces_ExcludesMould(t *testing.T) {
	c := &DeployConfig{Spaces: map[string]SpaceSpec{
		"production": {},
		"staging":    {},
		"ephemeral":  {}, // the mould — must not appear
	}}
	got := c.NamedSpaces()
	if !slices.Equal(got, []string{"production", "staging"}) {
		t.Errorf("NamedSpaces = %v, want [production staging] (sorted, mould excluded)", got)
	}
}

// A config with no `spaces:` block has no named spaces (any name resolves to the
// single base space), so NamedSpaces is empty — `--all` then has nothing to do.
func TestNamedSpaces_NoSpacesBlock(t *testing.T) {
	c := &DeployConfig{}
	if got := c.NamedSpaces(); len(got) != 0 {
		t.Errorf("NamedSpaces = %v, want empty when no spaces: block", got)
	}
}
