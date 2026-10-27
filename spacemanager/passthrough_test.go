package spacemanager

import (
	"strings"
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/ingress"
)

func TestPassthroughRoute_WholeHostResolves(t *testing.T) {
	cfg, err := parseFront(t, `
        - space: chat-production
          host: turn.chat.eco
          passthrough: 5349
`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, meta, err := cfg.Resolve("front", "singleton")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	r := findResolvedRoute(t, meta, "chat-production/"+ingress.GatewayService)
	if r.Passthrough != 5349 || r.Host != "turn.chat.eco" {
		t.Fatalf("resolved = %+v", r)
	}
}

func TestPassthroughRoute_ServiceRoute(t *testing.T) {
	cfg, err := Parse([]byte(`
services:
  livekit:
    image: livekit:local
    port: 7880
spaces:
  production:
    gateway:
      routes:
        - service: livekit
          host: meet.chat.eco
        - service: livekit
          host: turn.chat.eco
          passthrough: 5349
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, meta, err := cfg.Resolve("production", "singleton")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	var got []int
	for _, r := range meta.Routes {
		got = append(got, r.Passthrough)
	}
	if len(got) != 2 || got[0] != 0 || got[1] != 5349 {
		t.Fatalf("passthrough ports = %v", got)
	}
}

func TestPassthroughRoute_RefusesHTTPOptions(t *testing.T) {
	for _, extra := range []string{"redirect_apex: true", "timeout: 30s", "compress: true"} {
		_, err := parseFront(t, `
        - service: meshauthority
          host: turn.chat.eco
          passthrough: 5349
          `+extra+`
`)
		if err == nil || !strings.Contains(err.Error(), "TLS passthrough") {
			t.Fatalf("%q accepted on a passthrough route: %v", extra, err)
		}
	}
}

func TestPassthroughRoute_RefusesBadPortAndMeshService(t *testing.T) {
	for _, route := range []string{
		"- service: meshauthority\n          host: turn.chat.eco\n          passthrough: 70000",
		"- service: meshauthority\n          host: turn.chat.eco\n          passthrough: 6464",
		"- space: chat-production\n          host: turn.chat.eco\n          passthrough: 53",
		"- service: chat-production/livekit\n          host: turn.chat.eco\n          passthrough: 5349",
	} {
		if _, err := parseFront(t, "\n        "+route+"\n"); err == nil {
			t.Fatalf("accepted: %s", route)
		}
	}
}

func TestGateway_MeshPassthroughTargetsPortOnPeer(t *testing.T) {
	gw := newFakeGateway()
	resolve := func(space, service string) (string, bool) {
		return "10.90.0.9", space == "chat-production" && service == ingress.GatewayService
	}
	desired := []Service{{Name: "local", Command: []string{"sh", "-c", "sleep 30"}}}
	key := "chat-production/" + ingress.GatewayService
	a, cancel, wg := startNewAgent(t, newCounting(), nil, desired, meshOptsFor(gw, resolve,
		ResolvedRoute{Service: key, Host: "meet.chat.eco"},
		ResolvedRoute{Service: key, Host: "turn.chat.eco", Passthrough: 5349},
	))
	defer wg.Wait()
	defer cancel()

	a.RefreshMeshRoutes()
	waitFor(t, 2*time.Second, "passthrough declared", func() bool { return gw.routeCount("turn.chat.eco") >= 1 })
	d, _ := gw.lastRoute("turn.chat.eco")
	if !d.Passthrough || len(d.Backends) != 1 || d.Backends[0] != "10.90.0.9:5349" {
		t.Fatalf("passthrough domain = %+v", d)
	}
	h, _ := gw.lastRoute("meet.chat.eco")
	if h.Passthrough || h.Backends[0] != "10.90.0.9:80" {
		t.Fatalf("http domain = %+v", h)
	}
}
