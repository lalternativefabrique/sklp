package space

import (
	"strings"
	"testing"
)

func TestAgentSpaceNameIsStablePerCheckout(t *testing.T) {
	if AgentSpaceName("/a/partage") != AgentSpaceName("/a/partage") || AgentSpaceName("/a/partage") == AgentSpaceName("/b/partage") {
		t.Fatal("a checkout must keep its space, and two checkouts must not share one")
	}
	if !strings.HasPrefix(AgentSpaceName("/a/partage"), "agent-partage-") {
		t.Fatalf("name %q does not say which repository it is", AgentSpaceName("/a/partage"))
	}
}

func TestAgentSecretFilesAreFoundByName(t *testing.T) {
	for name, want := range map[string]bool{
		".env": true, ".env.secret": true, ".env.local": true, ".netrc": true, "tls.key": true, "id_ed25519": true,
		".env.example": false, "id_ed25519.pub": false, "main.go": false, "keys.go": false,
	} {
		if got := isAgentSecretFile(name); got != want {
			t.Errorf("isAgentSecretFile(%q) = %v, want %v", name, got, want)
		}
	}
}
