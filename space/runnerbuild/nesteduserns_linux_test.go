//go:build linux

package runnerbuild

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// TestNestedUserns_ReadsUidMap pins the detector against this machine's real
// /proc/self/uid_map. It asserts the rule, not a fixed answer, because the suite
// runs both on a workstation (identity map) and inside the CI runner's user
// namespace (a narrow one).
func TestNestedUserns_ReadsUidMap(t *testing.T) {
	b, err := os.ReadFile("/proc/self/uid_map")
	if err != nil {
		t.Skipf("no /proc/self/uid_map: %v", err)
	}
	f := strings.Fields(string(b))
	identity := len(f) >= 3 && f[2] == "4294967295"
	if got := NestedUserns(); got == identity {
		t.Fatalf("NestedUserns()=%v for uid_map %q (identity=%v)", got, strings.TrimSpace(string(b)), identity)
	}
}

// TestResolveNestingAdoptsDetection is the regression guard for the bug this
// fixes: every production caller built an UnshareStepRunner without setting
// NestedUserns, so the whole nested code path — apt.conf, resolv.conf copy,
// best-effort /proc — was dead code and CI builds died in dpkg. A runner that
// was not told must adopt whatever the environment reports.
func TestResolveNestingAdoptsDetection(t *testing.T) {
	for _, detected := range []bool{true, false} {
		u := UnshareStepRunner{detectNested: func() bool { return detected }}.resolveNesting()
		if u.NestedUserns != detected {
			t.Fatalf("detected=%v: NestedUserns=%v", detected, u.NestedUserns)
		}
	}
}

// TestExplicitNestedSurvivesResolve keeps the field usable as an override, so a
// caller that knows it is nested is never talked out of it by the probe.
func TestExplicitNestedSurvivesResolve(t *testing.T) {
	u := UnshareStepRunner{NestedUserns: true, detectNested: func() bool { return false }}.resolveNesting()
	if !u.NestedUserns {
		t.Fatal("explicit NestedUserns=true was overwritten by detection")
	}
}

// TestDetectedNestingReachesArgsAndScript ties detection to the two things that
// actually broke apt: the subuid range on the unshare command line, and the apt
// sandbox opt-out in the script.
func TestDetectedNestingReachesArgsAndScript(t *testing.T) {
	const subuidRange = "--map-users=1:100000:65535"

	nested := UnshareStepRunner{detectNested: func() bool { return true }}.resolveNesting()
	if !nested.NestedUserns {
		t.Fatal("detected nested did not resolve")
	}
	if args := unshareBuildArgs("true", nested.NestedUserns); slices.Contains(args, subuidRange) {
		t.Fatalf("nested args must not map a subuid range: %v", args)
	}
	if script := nested.buildScript("/rf", []string{"true"}); !strings.Contains(script, `APT::Sandbox::User "root"`) {
		t.Fatalf("nested script missing apt sandbox opt-out:\n%s", script)
	}

	host := UnshareStepRunner{detectNested: func() bool { return false }}.resolveNesting()
	if host.NestedUserns {
		t.Fatal("detected host resolved to nested")
	}
	if args := unshareBuildArgs("true", host.NestedUserns); !slices.Contains(args, subuidRange) {
		t.Fatalf("host args must map the subuid range: %v", args)
	}
}
