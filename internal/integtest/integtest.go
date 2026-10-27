// Package integtest gates the host-dependent integration tier (real
// userns/netns, runc, cgroups). Those tests t.Skip on hosts that lack the
// capability — correct for laptops and locked-down sandboxes, but it makes a
// silent no-op indistinguishable from a real pass: if the CI runner ever loses
// userns, every test skips and the step still goes green.
//
// Demand closes that gap. A runner that is KNOWN to be capable sets
// SKLP_REQUIRE_INTEG=1; on that runner a skip is a bug, so Demand turns it into
// a failure instead. Everywhere else (env unset) Demand is a no-op and the
// normal skip stands.
package integtest

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

// RequireEnv is the env var a known-capable runner sets to forbid skips.
const RequireEnv = "SKLP_REQUIRE_INTEG"

// Required reports whether this runner forbids skipping the integration tier.
func Required() bool { return os.Getenv(RequireEnv) == "1" }

// Demand is called by a capability probe right before it would t.Skip. When the
// runner has declared itself capable (SKLP_REQUIRE_INTEG=1) it fails instead,
// surfacing the lost capability rather than hiding it behind a green skip. The
// reason mirrors the probe's own skip message so the failure is self-explaining.
func Demand(t *testing.T, reason string) {
	t.Helper()
	if Required() {
		t.Fatalf("%s=1 but this test would skip: %s\n"+
			"the runner declared itself capable; a skip here means the capability was lost",
			RequireEnv, reason)
	}
}

// SkipUnlessRootlessBuild skips when this host cannot run the rootless build
// path: a user namespace with a subuid range, plus the mount and pid
// namespaces the unshare step runner adds. The probe runs the exact flag set
// the runner uses — a guard that checks less (no --pid) passes inside a
// Space on a CI node, where `unshare --user --pid` is EINVAL, and the tests
// then hang on the real call instead of skipping.
func SkipUnlessRootlessBuild(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "unshare", "--user", "--map-root-user",
		"--map-users=1:100000:65535", "--map-groups=1:100000:65535",
		"--mount", "--pid", "--fork", "true").CombinedOutput()
	if err != nil {
		reason := "rootless userns+subuid+pid namespaces unavailable: " + err.Error() + ": " + string(out)
		Demand(t, reason)
		t.Skip(reason)
	}
}
