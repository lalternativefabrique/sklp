package spacemanager

import (
	"strings"
	"testing"
)

func TestParsePortMappingProtocol(t *testing.T) {
	cases := map[string]PortMapping{
		"0.0.0.0:7882:7882/udp": {BindHost: "0.0.0.0", Host: 7882, Container: 7882, UDP: true},
		"0.0.0.0:443:5349/tcp":  {BindHost: "0.0.0.0", Host: 443, Container: 5349},
		"3478:3478":             {Host: 3478, Container: 3478},
	}
	for spec, want := range cases {
		got, err := parsePortMapping(spec)
		if err != nil || got != want {
			t.Errorf("parsePortMapping(%q) = %+v, %v; want %+v", spec, got, err, want)
		}
	}
	for _, bad := range []string{"7882:7882/sctp", "7882:7882/", "7882/udp"} {
		if _, err := parsePortMapping(bad); err == nil {
			t.Errorf("parsePortMapping(%q) accepted", bad)
		}
	}
}

func TestProxyProtocolOnlyFlagsTCP(t *testing.T) {
	maps, err := parsePortMappings([]string{"3478:3478/udp", "3478:3478"})
	if err != nil {
		t.Fatal(err)
	}
	maps, err = applyProxyProtocol(maps, []int{3478})
	if err != nil {
		t.Fatal(err)
	}
	if maps[0].ProxyProtocol || !maps[1].ProxyProtocol {
		t.Fatalf("proxy protocol flags = %v/%v, want udp off, tcp on", maps[0].ProxyProtocol, maps[1].ProxyProtocol)
	}

	udpOnly, _ := parsePortMappings([]string{"3478:3478/udp"})
	if _, err := applyProxyProtocol(udpOnly, []int{3478}); err == nil || !strings.Contains(err.Error(), "TCP") {
		t.Fatalf("proxy_protocol on a udp-only port accepted: %v", err)
	}
}
