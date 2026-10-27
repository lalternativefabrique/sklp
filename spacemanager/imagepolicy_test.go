package spacemanager

import "testing"

func TestImagePolicy_UnsetAcceptsAnything(t *testing.T) {
	var p ImagePolicy
	for _, img := range []string{"busybox:latest", "rg.fr-par.scw.cloud/skalpai/core:1", "evil.example/x@sha256:0000000000000000000000000000000000000000000000000000000000000000"} {
		if err := p.Check(img); err != nil {
			t.Errorf("Check(%q) = %v, want nil", img, err)
		}
	}
}

func TestImagePolicy_TrustedRegistries(t *testing.T) {
	p := ImagePolicy{TrustedRegistries: []string{"rg.fr-par.scw.cloud", "docker.io"}}
	for _, img := range []string{"rg.fr-par.scw.cloud/skalpai/core:1", "nats:2.10-alpine", "docker.io/library/postgres:16", "RG.FR-PAR.SCW.CLOUD/skalpai/web:1"} {
		if err := p.Check(img); err != nil {
			t.Errorf("Check(%q) = %v, want accepted", img, err)
		}
	}
	for _, img := range []string{"ghcr.io/x/y:1", "evil.example/busybox", "localhost:5000/x", "not a ref"} {
		if err := p.Check(img); err == nil {
			t.Errorf("Check(%q) accepted, want refused", img)
		}
	}
}

func TestImagePolicy_RequireDigest(t *testing.T) {
	p := ImagePolicy{RequireDigest: true}
	if err := p.Check("docker.io/library/alpine@sha256:0000000000000000000000000000000000000000000000000000000000000000"); err != nil {
		t.Errorf("digest-pinned refused: %v", err)
	}
	if err := p.Check("alpine:3.20"); err == nil {
		t.Error("tag-only accepted under RequireDigest")
	}
}

func TestImagePolicyFromEnv(t *testing.T) {
	t.Setenv(TrustedRegistriesEnv, " rg.fr-par.scw.cloud, Docker.io ,,")
	t.Setenv(RequireImageDigestEnv, "1")
	p := ImagePolicyFromEnv()
	if len(p.TrustedRegistries) != 2 || p.TrustedRegistries[0] != "rg.fr-par.scw.cloud" || p.TrustedRegistries[1] != "docker.io" || !p.RequireDigest {
		t.Fatalf("ImagePolicyFromEnv = %+v", p)
	}
}
