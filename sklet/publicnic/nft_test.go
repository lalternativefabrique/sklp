package publicnic

import (
	"reflect"
	"strings"
	"testing"
)

const listing = `table ip sklp-smtp-egress {
	chain output {
		type route hook output priority mangle; policy accept;
		ip daddr != { 10.0.0.0/8, 127.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16 } tcp dport 25 meta mark set 0x00000019
	}

	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		oifname "pub0" snat ip to 203.0.113.213
	}
}
`

func TestNftMissing(t *testing.T) {
	cases := []struct {
		name    string
		listing string
		want    []string
	}{
		{"complete", listing, nil},
		{"older nft snat syntax", strings.Replace(listing, "snat ip to", "snat to", 1), nil},
		{"absent table", "", []string{"table"}},
		{"other source", strings.Replace(listing, "203.0.113.213", "203.0.113.214", 1), []string{"snat"}},
		{"other mark", strings.Replace(listing, "0x00000019", "0x00000018", 1), []string{"smtp-mark"}},
		{"no private exclusion", strings.Replace(listing, "ip daddr != { 10.0.0.0/8, 127.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16 } ", "", 1), []string{"smtp-mark"}},
		{"flushed chains", "table ip sklp-smtp-egress {\n}\n", []string{"output-chain", "smtp-mark", "postrouting-chain", "snat"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := NftMissing(c.listing, desiredFixture()); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestNftScriptIsOneTransaction(t *testing.T) {
	s := NftScript(desiredFixture())
	for _, want := range []string{
		"flush table ip sklp-smtp-egress",
		"tcp dport 25 meta mark set 0x19",
		`oifname "pub0" snat to 203.0.113.213`,
		"priority srcnat",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("script lacks %q:\n%s", want, s)
		}
	}
}
