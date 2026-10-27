package spacemanager

import (
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/ingress"
)

func parseFront(t *testing.T, routes string) (*DeployConfig, error) {
	t.Helper()
	return Parse([]byte(`
services:
  meshauthority:
    image: ma:local
    port: 4300

spaces:
  front:
    gateway:
      routes:
` + routes))
}

func TestWholeHostRoute_ResolvesToSpaceGateway(t *testing.T) {
	cfg, err := parseFront(t, `
        - space: shop-production
          host: shop.fr
          redirect_apex: true
`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, meta, err := cfg.Resolve("front", "singleton")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	r := findResolvedRoute(t, meta, "shop-production/"+ingress.GatewayService)
	if r.Host != "shop.fr" || r.Path != "" || !r.RedirectApex {
		t.Fatalf("resolved whole-host route = %+v", r)
	}
}

func TestWholeHostRoute_RefusesPathAndOptions(t *testing.T) {
	for _, extra := range []string{"path: /api", "kind: api", "rate: 5\n          burst: 10", "timeout: 30s", "max_body: 1Mi", "security_headers: true", "compress: true"} {
		_, err := parseFront(t, `
        - space: shop-production
          host: shop.fr
          `+extra+`
`)
		if err == nil || !strings.Contains(err.Error(), "hands it whole") {
			t.Fatalf("%q accepted on a whole-host route: %v", extra, err)
		}
	}
}

func TestWholeHostRoute_SpaceAndServiceExclusive(t *testing.T) {
	_, err := parseFront(t, `
        - space: shop-production
          service: meshauthority
          host: shop.fr
`)
	if err == nil || !strings.Contains(err.Error(), "both space and service") {
		t.Fatalf("space+service accepted: %v", err)
	}
}

func TestWholeHostRoute_OneHostOneSpace(t *testing.T) {
	_, err := parseFront(t, `
        - space: shop-production
          host: shop.fr
        - space: partage-production
          host: shop.fr
`)
	if err == nil || !strings.Contains(err.Error(), "exactly one space") {
		t.Fatalf("host handed to two spaces accepted: %v", err)
	}
}

func TestWholeHostRoute_NoMixWithDetailedRoutes(t *testing.T) {
	for _, routes := range []string{`
        - space: shop-production
          host: shop.fr
        - service: shop-production/core
          host: shop.fr
          path: /api
`, `
        - service: shop-production/core
          host: shop.fr
          path: /api
        - space: shop-production
          host: shop.fr
`} {
		_, err := parseFront(t, routes)
		if err == nil || !strings.Contains(err.Error(), "not both") {
			t.Fatalf("mixed forms on one host accepted: %v", err)
		}
	}
}

func TestWholeHostRoute_DetailedFormStillWorks(t *testing.T) {
	cfg, err := parseFront(t, `
        - service: shop-production/core
          host: api.example.com
          kind: api
        - space: shop-production
          host: shop.fr
`)
	if err != nil {
		t.Fatalf("detailed and whole-host routes on distinct hosts refused: %v", err)
	}
	_, meta, err := cfg.Resolve("front", "singleton")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if findResolvedRoute(t, meta, "shop-production/core").Host != "api.example.com" {
		t.Fatal("detailed route lost")
	}
}
