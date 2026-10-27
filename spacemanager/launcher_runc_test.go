package spacemanager

import (
	"testing"

	"github.com/lalternativefabrique/sklp/space"
)

func TestRuncLauncherSetRegistryAuthsMerges(t *testing.T) {
	l := &runcLauncher{}
	l.SetRegistryAuths(map[string]space.RegistryAuth{"rg.example": {Username: "nologin", Password: "s3cret"}})

	l.SetRegistryAuths(nil)
	l.SetRegistryAuths(map[string]space.RegistryAuth{})
	if got := l.auths["rg.example"].Password; got != "s3cret" {
		t.Fatalf("an empty apply wiped the credentials: %q", got)
	}

	l.SetRegistryAuths(map[string]space.RegistryAuth{"ghcr.io": {Username: "u", Password: "p"}})
	if len(l.auths) != 2 || l.auths["rg.example"].Password != "s3cret" {
		t.Fatalf("a second registry must not drop the first: %v", l.auths)
	}

	l.SetRegistryAuths(map[string]space.RegistryAuth{"rg.example": {Username: "nologin", Password: "rotated"}})
	if got := l.auths["rg.example"].Password; got != "rotated" {
		t.Fatalf("a known host must take the new password, got %q", got)
	}
}
