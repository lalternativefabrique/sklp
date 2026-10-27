package spacenet

import (
	"strings"
	"testing"
)

func TestHostGuardRules_ControllerPortsOnlyEveryOtherHostPortDropped(t *testing.T) {
	got := hostGuardRules("tap0", []string{"10.99.0.10", "79.137.18.6/32"}, []string{"10.99.0.10"}, []int{5432, 80, 53, 80, 0, 70000})
	for _, want := range []string{
		"flush table ip sklp_hostguard",
		"hook forward priority -10",
		"ct state established,related accept",
		`oifname "tap0" ip daddr { 10.99.0.10 } udp dport 53 accept`,
		`oifname "tap0" ip daddr { 10.99.0.10 } tcp dport { 53, 80, 5432 } accept`,
		`oifname "tap0" ip daddr { 10.99.0.10, 79.137.18.6 } drop`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rules lack %q:\n%s", want, got)
		}
	}
	if strings.Index(got, "tcp dport") > strings.Index(got, " drop") {
		t.Fatalf("the controller accept must come before the drop:\n%s", got)
	}
	for _, port := range []string{" 22", "6464", "70000"} {
		if strings.Contains(got, "dport { "+strings.TrimSpace(port)) || strings.Contains(got, ", "+strings.TrimSpace(port)+" ") {
			t.Errorf("port %s must not be allowed:\n%s", port, got)
		}
	}
}

func TestHostGuardRules_NoControllerDropsEveryHostAddress(t *testing.T) {
	got := hostGuardRules("tap0", []string{"10.99.0.10"}, nil, []int{5432})
	if strings.Contains(got, "accept\n") && strings.Contains(got, "dport") {
		t.Fatalf("without a controller nothing on the host may be allowed:\n%s", got)
	}
	if !strings.Contains(got, `ip daddr { 10.99.0.10 } drop`) {
		t.Fatalf("host address not dropped:\n%s", got)
	}
}

func TestHostGuardRules_PublicAddressOnlyGatewayPorts(t *testing.T) {
	got := hostGuardRulesWithPublic("tap0", []string{"10.99.0.10"}, []string{"10.99.0.10"}, []int{5432}, []string{"79.137.18.6"})
	for _, want := range []string{
		`oifname "tap0" ip daddr { 79.137.18.6 } tcp dport { 80, 443 } accept`,
		`oifname "tap0" ip daddr { 79.137.18.6 } drop`,
		`oifname "tap0" ip daddr { 10.99.0.10 } drop`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rules lack %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "79.137.18.6 } tcp dport { 5432") {
		t.Fatalf("the controller's service ports must not open the public address:\n%s", got)
	}
}
