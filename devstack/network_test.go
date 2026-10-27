package devstack

import (
	"reflect"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/pipeline"
)

func TestResolveNetwork(t *testing.T) {
	cases := []struct {
		in       string
		wantMode NetworkMode
		warnHas  string // substring expected in warn ("" = no warn)
	}{
		{"", DefaultNetworkMode, ""},
		{"bridge", NetworkBridge, ""},
		{"BRIDGE", NetworkBridge, ""},
		{"host", NetworkHost, "deprecated"},
		{"weird", NetworkHost, "unknown network"},
	}
	for _, c := range cases {
		mode, warn := resolveNetwork(pipeline.Service{Network: c.in})
		if mode != c.wantMode {
			t.Errorf("%q: mode = %v, want %v", c.in, mode, c.wantMode)
		}
		if c.warnHas == "" && warn != "" {
			t.Errorf("%q: unexpected warn %q", c.in, warn)
		}
		if c.warnHas != "" && !strings.Contains(warn, c.warnHas) {
			t.Errorf("%q: warn %q missing %q", c.in, warn, c.warnHas)
		}
	}
}

func TestNetworkName_TruncatesLongID(t *testing.T) {
	long := strings.Repeat("a", 64)
	got := networkName("dev", long)
	if len(got) > 32 {
		t.Errorf("expected truncated name, got len=%d (%q)", len(got), got)
	}
	if !strings.HasPrefix(got, "sklp-dev-") {
		t.Errorf("missing prefix: %q", got)
	}
}

func TestMergeEnv_OverlayWins(t *testing.T) {
	base := map[string]string{"A": "1", "B": "2"}
	overlay := map[string]string{"B": "x", "C": "3"}
	got := mergeEnv(base, overlay)
	want := map[string]string{"A": "1", "B": "x", "C": "3"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mergeEnv = %v, want %v", got, want)
	}
}
