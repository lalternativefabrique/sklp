package publicnic

import (
	"fmt"
	"net"
)

type Desired struct {
	Interface string `json:"interface"`
	LinkIndex int    `json:"link_index"`
	Gateway   net.IP `json:"gateway"`
	Source    net.IP `json:"source"`
	Table     int    `json:"table"`
	Mark      uint32 `json:"mark"`
}

type RouteState struct {
	Gateway   net.IP `json:"gateway"`
	LinkIndex int    `json:"link_index"`
	OnLink    bool   `json:"onlink"`
}

type Observed struct {
	DefaultRoute *RouteState `json:"default_route"`
	RuleFrom     bool        `json:"rule_from"`
	RuleMark     bool        `json:"rule_fwmark"`
	NftMissing   []string    `json:"nft_missing"`
}

type Action string

const (
	ActionRoute    Action = "route"
	ActionRuleFrom Action = "rule-from"
	ActionRuleMark Action = "rule-fwmark"
	ActionNft      Action = "nft"
)

type Repair struct {
	Action Action `json:"action"`
	Detail string `json:"detail"`
}

func Resolve(cfg Config, linkIndex int, lease Lease) (Desired, error) {
	cfg = cfg.WithDefaults()
	src := lease.Address
	if cfg.Source != "" {
		src = net.ParseIP(cfg.Source).To4()
	}
	if src == nil {
		return Desired{}, fmt.Errorf("no source address for %s", cfg.Interface)
	}
	if lease.Router == nil {
		return Desired{}, fmt.Errorf("no gateway for %s", cfg.Interface)
	}
	return Desired{
		Interface: cfg.Interface,
		LinkIndex: linkIndex,
		Gateway:   lease.Router,
		Source:    src,
		Table:     cfg.Table,
		Mark:      cfg.Mark,
	}, nil
}

func Plan(d Desired, o Observed) []Repair {
	var out []Repair
	if detail, ok := routeDrift(d, o.DefaultRoute); !ok {
		out = append(out, Repair{ActionRoute, detail})
	}
	if !o.RuleFrom {
		out = append(out, Repair{ActionRuleFrom, fmt.Sprintf("from %s table %d absent", d.Source, d.Table)})
	}
	if !o.RuleMark {
		out = append(out, Repair{ActionRuleMark, fmt.Sprintf("fwmark %#x table %d absent", d.Mark, d.Table)})
	}
	if len(o.NftMissing) > 0 {
		out = append(out, Repair{ActionNft, fmt.Sprintf("table ip %s missing %v", NftTable, o.NftMissing)})
	}
	return out
}

func routeDrift(d Desired, r *RouteState) (string, bool) {
	want := fmt.Sprintf("default via %s dev %s onlink table %d", d.Gateway, d.Interface, d.Table)
	if r == nil {
		return want + " absent", false
	}
	if !r.Gateway.Equal(d.Gateway) || r.LinkIndex != d.LinkIndex || !r.OnLink {
		return fmt.Sprintf("%s, found via %s ifindex %d onlink=%t", want, r.Gateway, r.LinkIndex, r.OnLink), false
	}
	return "", true
}
