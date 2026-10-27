package publicnic

import (
	"net"
	"strings"
	"testing"
)

func desiredFixture() Desired {
	return Desired{Interface: "pub0", LinkIndex: 3, Gateway: net.ParseIP("198.51.100.1").To4(), Source: net.ParseIP("203.0.113.213").To4(), Table: 100, Mark: 0x19}
}

func healthy() Observed {
	return Observed{DefaultRoute: &RouteState{Gateway: net.ParseIP("198.51.100.1"), LinkIndex: 3, OnLink: true}, RuleFrom: true, RuleMark: true}
}

func TestResolve(t *testing.T) {
	lease := Lease{Address: net.ParseIP("198.51.100.34").To4(), Router: net.ParseIP("198.51.100.1").To4()}
	d, err := Resolve(Config{}, 3, lease)
	if err != nil {
		t.Fatal(err)
	}
	if d.Source.String() != "198.51.100.34" || d.Interface != "pub0" || d.Table != 100 || d.Mark != 0x19 {
		t.Fatalf("defaults: %+v", d)
	}
	d, err = Resolve(Config{Source: "203.0.113.213"}, 3, lease)
	if err != nil || d.Source.String() != "203.0.113.213" {
		t.Fatalf("additional ip: %+v %v", d, err)
	}
	if _, err := Resolve(Config{}, 3, Lease{Address: lease.Address}); err == nil {
		t.Fatal("no gateway must fail")
	}
}

func TestPlan(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Observed)
		want   []Action
	}{
		{"healthy", func(*Observed) {}, nil},
		{"route absent", func(o *Observed) { o.DefaultRoute = nil }, []Action{ActionRoute}},
		{"wrong gateway", func(o *Observed) { o.DefaultRoute.Gateway = net.ParseIP("198.51.100.9") }, []Action{ActionRoute}},
		{"wrong link", func(o *Observed) { o.DefaultRoute.LinkIndex = 2 }, []Action{ActionRoute}},
		{"not onlink", func(o *Observed) { o.DefaultRoute.OnLink = false }, []Action{ActionRoute}},
		{"rule from absent", func(o *Observed) { o.RuleFrom = false }, []Action{ActionRuleFrom}},
		{"rule mark absent", func(o *Observed) { o.RuleMark = false }, []Action{ActionRuleMark}},
		{"nft drift", func(o *Observed) { o.NftMissing = []string{"snat"} }, []Action{ActionNft}},
		{"networkd flush", func(o *Observed) {
			*o = Observed{NftMissing: []string{"table"}}
		}, []Action{ActionRoute, ActionRuleFrom, ActionRuleMark, ActionNft}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := healthy()
			c.mutate(&o)
			got := Plan(desiredFixture(), o)
			if len(got) != len(c.want) {
				t.Fatalf("got %+v, want %v", got, c.want)
			}
			for i := range got {
				if got[i].Action != c.want[i] || got[i].Detail == "" {
					t.Fatalf("repair %d = %+v, want %s", i, got[i], c.want[i])
				}
			}
		})
	}
}

func TestPlanDetailNamesWhatIsMissing(t *testing.T) {
	o := healthy()
	o.RuleMark = false
	got := Plan(desiredFixture(), o)
	if !strings.Contains(got[0].Detail, "fwmark 0x19 table 100") {
		t.Fatalf("detail = %q", got[0].Detail)
	}
}
