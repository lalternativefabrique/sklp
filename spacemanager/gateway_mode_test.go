package spacemanager

import (
	"strings"
	"testing"
)

// TestValidateGatewayMode is consumed by `sklet agent --gateway-mode` only: a
// deploy.yaml can no longer declare a mode (see TestParse_RejectsGatewayMode).
func TestValidateGatewayMode(t *testing.T) {
	for _, mode := range []string{GatewayModeSingleton, GatewayModePerSpace} {
		if err := ValidateGatewayMode(mode); err != nil {
			t.Fatalf("ValidateGatewayMode(%q) = %v, want nil", mode, err)
		}
	}
	for _, mode := range []string{"", "traefik", "perspace", "PER-SPACE"} {
		if err := ValidateGatewayMode(mode); err == nil {
			t.Fatalf("ValidateGatewayMode(%q) = nil, want an error", mode)
		}
	}
}

// TestParse_RejectsGatewayMode: gateway.mode is no longer a deploy.yaml field
// (the front-door mode is node-owned, set exclusively by `sklet agent
// --gateway-mode`). A deploy.yaml that still declares it must fail parsing
// loudly instead of silently dropping the key (yaml.Unmarshal's default lenient
// behaviour) — the config would otherwise look like it selects a mode but does
// nothing.
func TestParse_RejectsGatewayMode(t *testing.T) {
	y := "gateway:\n  mode: per-space\nservices:\n  core:\n    image: img\n"
	_, err := Parse([]byte(y))
	if err == nil || !strings.Contains(err.Error(), "gateway.mode") {
		t.Fatalf("Parse with gateway.mode: expected a rejection error, got %v", err)
	}
}
