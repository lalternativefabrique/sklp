package spacemanager

import (
	"strings"
	"testing"
)

func TestParse_GatewayDriverDefaultsWhenAbsent(t *testing.T) {
	y := "services:\n  core:\n    image: img\n"
	cfg, err := Parse([]byte(y))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.GatewayDriverName(); got != DefaultGatewayDriver {
		t.Errorf("GatewayDriverName() = %q, want default %q", got, DefaultGatewayDriver)
	}
}

func TestParse_GatewayDriverExplicit(t *testing.T) {
	y := "gateway:\n  driver: traefik\nservices:\n  core:\n    image: img\n"
	cfg, err := Parse([]byte(y))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.GatewayDriverName(); got != "traefik" {
		t.Errorf("GatewayDriverName() = %q, want traefik", got)
	}
}

func TestParse_GatewayUnknownDriverIsError(t *testing.T) {
	y := "gateway:\n  driver: nope\nservices:\n  core:\n    image: img\n"
	_, err := Parse([]byte(y))
	if err == nil || !strings.Contains(err.Error(), "gateway") {
		t.Fatalf("Parse with unknown gateway driver: err = %v, want a gateway error", err)
	}
}
