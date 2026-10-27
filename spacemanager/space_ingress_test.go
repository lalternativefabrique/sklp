package spacemanager

import (
	"strings"
	"testing"
)

// findResolvedService returns the resolved service by name, failing if absent.
func findResolvedService(t *testing.T, svcs []Service, name string) Service {
	t.Helper()
	for _, s := range svcs {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("service %q not found in resolved set", name)
	return Service{}
}

// parseIngressFixture parses a YAML body and fails on error.
func parseIngressFixture(t *testing.T, body string) *DeployConfig {
	t.Helper()
	cfg, err := Parse([]byte(body))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return cfg
}

const ingressTwoSpacesYAML = `
services:
  core:
    image: core:latest
    expose: "4000"

spaces:
  alpha:
    ingress:
      core: "4100:4000"
  beta:
    ingress:
      core: "4101:4000"
`

func TestIngress_PerSpaceMapsToServicePorts(t *testing.T) {
	cfg := parseIngressFixture(t, ingressTwoSpacesYAML)

	alpha, _, err := cfg.Resolve("alpha", GatewayModeSingleton)
	if err != nil {
		t.Fatalf("Resolve(alpha): %v", err)
	}
	if got := findResolvedService(t, alpha, "core").Ports; len(got) != 1 || got[0] != "4100:4000" {
		t.Fatalf("alpha core ports: want [4100:4000], got %v", got)
	}

	beta, _, err := cfg.Resolve("beta", GatewayModeSingleton)
	if err != nil {
		t.Fatalf("Resolve(beta): %v", err)
	}
	if got := findResolvedService(t, beta, "core").Ports; len(got) != 1 || got[0] != "4101:4000" {
		t.Fatalf("beta core ports: want [4101:4000], got %v", got)
	}
}

func TestIngress_HostPortReusedAcrossSpaces_Errors(t *testing.T) {
	body := `
services:
  core:
    image: core:latest
    expose: "4000"

spaces:
  alpha:
    ingress:
      core: "4100:4000"
  beta:
    ingress:
      core: "4100:4000"
`
	_, err := Parse([]byte(body))
	if err == nil {
		t.Fatal("want host-port-reuse error, got nil")
	}
	if !strings.Contains(err.Error(), "4100") {
		t.Fatalf("error should name the conflicting host port 4100, got: %v", err)
	}
}

func TestIngress_UnknownService_Errors(t *testing.T) {
	body := `
services:
  core:
    image: core:latest
    expose: "4000"

spaces:
  alpha:
    ingress:
      nope: "4100:4000"
`
	_, err := Parse([]byte(body))
	if err == nil {
		t.Fatal("want unknown-service error, got nil")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Fatalf("error should name the unknown service, got: %v", err)
	}
}

func TestIngress_ContainerPortMismatchExpose_Errors(t *testing.T) {
	body := `
services:
  core:
    image: core:latest
    expose: "4000"

spaces:
  alpha:
    ingress:
      core: "4100:9999"
`
	_, err := Parse([]byte(body))
	if err == nil {
		t.Fatal("want container-port-mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "port") {
		t.Fatalf("error should mention port mismatch, got: %v", err)
	}
}

func TestIngress_EphemeralFixedHostPort_Errors(t *testing.T) {
	body := `
services:
  core:
    image: core:latest
    expose: "4000"

spaces:
  ephemeral:
    isolate: prefix
    url: "${SPACE}.preview.skalpai.io"
    ingress:
      core: "4100:4000"
`
	_, err := Parse([]byte(body))
	if err == nil {
		t.Fatal("want ephemeral-fixed-ingress error, got nil")
	}
	if !strings.Contains(err.Error(), "ephemeral") {
		t.Fatalf("error should mention ephemeral, got: %v", err)
	}
}

func TestIngress_OverridesServiceLevelPorts(t *testing.T) {
	body := `
services:
  svc:
    image: svc:latest
    expose: "4000"
    ports: ["9999:4000"]

spaces:
  alpha:
    ingress:
      svc: "4100:4000"
`
	cfg := parseIngressFixture(t, body)
	alpha, _, err := cfg.Resolve("alpha", GatewayModeSingleton)
	if err != nil {
		t.Fatalf("Resolve(alpha): %v", err)
	}
	if got := findResolvedService(t, alpha, "svc").Ports; len(got) != 1 || got[0] != "4100:4000" {
		t.Fatalf("alpha svc ports: want [4100:4000], got %v", got)
	}
}
