package spacemanager

import (
	"reflect"
	"strings"
	"testing"
)

func TestParsePortMapping(t *testing.T) {
	tests := []struct {
		name      string
		spec      string
		wantBind  string
		wantHost  int
		wantCtr   int
		wantError bool
	}{
		{name: "host:container", spec: "8080:80", wantHost: 8080, wantCtr: 80},
		{name: "ip:host:container binds ip", spec: "0.0.0.0:8080:80", wantBind: "0.0.0.0", wantHost: 8080, wantCtr: 80},
		{name: "loopback bind", spec: "127.0.0.1:8080:80", wantBind: "127.0.0.1", wantHost: 8080, wantCtr: 80},
		{name: "bind must be an ip", spec: "eth0:8080:80", wantError: true},
		{name: "whitespace tolerated", spec: " 8080 : 80 ", wantHost: 8080, wantCtr: 80},
		{name: "max ports", spec: "65535:65535", wantHost: 65535, wantCtr: 65535},
		{name: "empty", spec: "", wantError: true},
		{name: "bare port no colon", spec: "8080", wantError: true},
		{name: "missing host side", spec: ":80", wantError: true},
		{name: "missing container side", spec: "8080:", wantError: true},
		{name: "non-numeric host", spec: "abc:80", wantError: true},
		{name: "non-numeric container", spec: "8080:xyz", wantError: true},
		{name: "host out of range", spec: "70000:80", wantError: true},
		{name: "zero port", spec: "0:80", wantError: true},
		{name: "unknown proto suffix rejected", spec: "8080:80/sctp", wantError: true},
		{name: "too many colons", spec: "1:2:3:4", wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := parsePortMapping(tt.spec)
			if tt.wantError {
				if err == nil {
					t.Fatalf("parsePortMapping(%q) = %+v, want error", tt.spec, m)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePortMapping(%q): %v", tt.spec, err)
			}
			if m.BindHost != tt.wantBind || m.Host != tt.wantHost || m.Container != tt.wantCtr {
				t.Fatalf("parsePortMapping(%q) = %q:%d:%d, want %q:%d:%d", tt.spec, m.BindHost, m.Host, m.Container, tt.wantBind, tt.wantHost, tt.wantCtr)
			}
		})
	}
}

func TestParsePortMappings(t *testing.T) {
	if got, err := parsePortMappings(nil); err != nil || got != nil {
		t.Fatalf("parsePortMappings(nil) = %v, %v; want nil, nil", got, err)
	}
	got, err := parsePortMappings([]string{"8080:80", "9090:90"})
	if err != nil {
		t.Fatalf("parsePortMappings: %v", err)
	}
	if len(got) != 2 || got[0].Host != 8080 || got[1].Container != 90 {
		t.Fatalf("parsePortMappings = %+v", got)
	}
	// One malformed entry fails the whole list (no silent drop).
	if _, err := parsePortMappings([]string{"8080:80", "bad"}); err == nil {
		t.Fatal("parsePortMappings with a malformed entry: want error")
	}
}

func TestApplyProxyProtocol(t *testing.T) {
	maps, err := parsePortMappings([]string{"0.0.0.0:25:25", "0.0.0.0:80:8080", "443:443"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := applyProxyProtocol(maps, []int{25, 443})
	if err != nil {
		t.Fatalf("applyProxyProtocol: %v", err)
	}
	if !got[0].ProxyProtocol || got[1].ProxyProtocol || !got[2].ProxyProtocol {
		t.Fatalf("flags = %v %v %v, want true false true", got[0].ProxyProtocol, got[1].ProxyProtocol, got[2].ProxyProtocol)
	}
	if _, err := applyProxyProtocol(maps, []int{993}); err == nil {
		t.Fatal("port 993 has no mapping, want error")
	}
	if _, err := applyProxyProtocol(nil, nil); err != nil {
		t.Fatalf("empty: %v", err)
	}
}

func TestParsePortSpecRanges(t *testing.T) {
	cases := []struct {
		name    string
		spec    string
		want    []PortMapping
		wantErr string
	}{
		{name: "single unchanged", spec: "8080:80", want: []PortMapping{{Host: 8080, Container: 80}}},
		{name: "range", spec: "5000-5002:6000-6002", want: []PortMapping{
			{Host: 5000, Container: 6000}, {Host: 5001, Container: 6001}, {Host: 5002, Container: 6002}}},
		{name: "ip and udp", spec: "0.0.0.0:7882-7883:7882-7883/udp", want: []PortMapping{
			{BindHost: "0.0.0.0", Host: 7882, Container: 7882, UDP: true},
			{BindHost: "0.0.0.0", Host: 7883, Container: 7883, UDP: true}}},
		{name: "one-port range", spec: "9000-9000:9000-9000/tcp", want: []PortMapping{{Host: 9000, Container: 9000}}},
		{name: "cap inclusive", spec: "1-1000:1-1000", want: nil},
		{name: "mismatched lengths", spec: "5000-5002:6000-6001", wantErr: "host range has 3 ports, container range has 2"},
		{name: "range to single", spec: "5000-5002:6000", wantErr: "host range has 3 ports"},
		{name: "reversed", spec: "5002-5000:5002-5000", wantErr: "reversed"},
		{name: "over cap", spec: "1-1001:1-1001", wantErr: "max 1000"},
		{name: "bad proto", spec: "1-2:1-2/sctp", wantErr: "not supported"},
		{name: "bad ip", spec: "host:1-2:1-2", wantErr: "not an IP"},
		{name: "open end", spec: "5000-:5000-", wantErr: "missing"},
		{name: "out of range", spec: "65535-65536:1-2", wantErr: "out of range"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePortSpec(tc.spec)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.want == nil {
				if len(got) != 1000 {
					t.Fatalf("len = %d, want 1000", len(got))
				}
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParsePortMappingRejectsRange(t *testing.T) {
	if _, err := parsePortMapping("5000-5001:5000-5001"); err == nil || !strings.Contains(err.Error(), "range is not allowed") {
		t.Fatalf("err = %v", err)
	}
}
