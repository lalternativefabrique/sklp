package domain

import "testing"

func TestMatchCredential_LongestPrefixWins(t *testing.T) {
	creds := []*RegistryCredential{
		{Prefix: "registry.example.com/acme", Secret: "broad"},
		{Prefix: "registry.example.com/acme/core", Secret: "narrow"},
		{Prefix: "rg.fr-par.scw.cloud/shop", Secret: "other"},
	}
	if c := MatchCredential(creds, "registry.example.com/acme/core"); c == nil || c.Secret != "narrow" {
		t.Fatalf("want narrow, got %+v", c)
	}
	if c := MatchCredential(creds, "registry.example.com/acme/web"); c == nil || c.Secret != "broad" {
		t.Fatalf("want broad, got %+v", c)
	}
	if c := MatchCredential(creds, "docker.io/library/nginx"); c != nil {
		t.Fatalf("want nil for unmatched repo, got %+v", c)
	}
}
