//go:build linux

package space

import (
	"errors"
	"strings"
	"testing"
)

// runc reports "container's cgroup is not empty" and names neither the culprit
// nor a way out; the hint must supply both.
func TestLeakedCgroupHintOnCgroupError(t *testing.T) {
	err := errors.New(`runc run -d: exit status 1
--- container stdio ---
level=error msg="runc run failed: container's cgroup is not empty: 1 process(es) found"`)

	got := leakedCgroupHint(err, "sklp-deploy-ci-abc12345-nats-test")
	if got == "" {
		t.Fatal("no hint for a cgroup-not-empty error")
	}
	if !strings.Contains(got, "sklp clean") {
		t.Fatalf("hint %q does not point at a remedy", got)
	}
}

func TestLeakedCgroupHintIgnoresOtherErrors(t *testing.T) {
	if got := leakedCgroupHint(errors.New("no such image"), "sklp-deploy-x"); got != "" {
		t.Fatalf("hint = %q, want none for an unrelated error", got)
	}
	if got := leakedCgroupHint(nil, "sklp-deploy-x"); got != "" {
		t.Fatalf("hint = %q, want none for a nil error", got)
	}
}
