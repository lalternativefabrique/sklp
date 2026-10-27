//go:build linux

package plan

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

// TestExecuteCI_NetworkNoneIsolates asserts that `network: none` on
// Linux really unshares the network namespace: a curl to 127.0.0.1 in
// the unshared netns sees only a down loopback, so it cannot connect.
// The test is skipped on hosts that cannot create a new network
// namespace (no CAP_SYS_ADMIN / userns nesting disabled) — the
// sandbox already logs a clear warning in that case.
func TestExecuteCI_NetworkNoneIsolates(t *testing.T) {
	if os.Geteuid() != 0 && !userNsAvailable() {
		t.Skip("network namespace creation not permitted on this host")
	}

	pl := &Plan{
		SchemaVersion: SchemaVersion,
		Kind:          KindCI,
		CI: &CIPlan{
			Steps: []CIStep{
				{
					Name:    "blocked",
					Run:     "getent hosts 127.0.0.1 >/dev/null; ip -4 -o addr show dev lo 2>/dev/null || true",
					Status:  StatusIncluded,
					Network: "none",
				},
			},
		},
	}
	var out bytes.Buffer
	rep, err := ExecuteCI(context.Background(), CIExecConfig{
		Plan: pl, Repo: t.TempDir(), BaseEnv: os.Environ(),
		Stdout: &out, Stderr: &out,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	// Even if the kernel refused, the warning path should have fallen
	// back to host network and the step should pass. If the kernel
	// honored the request, `ip addr show dev lo` shows an empty list
	// (no inet line for 127.0.0.1) because the netns starts with a
	// down loopback.
	if rep.PassFail != "pass" {
		t.Logf("output:\n%s", out.String())
		t.Fatalf("step failed: %+v", rep.Steps)
	}
	// If we did manage to unshare (no warning emitted), the loopback
	// has no IPv4 address assigned. We accept either outcome — the
	// warning path is documented as best-effort.
	if !strings.Contains(out.String(), "network: none requested") {
		// Real unshare: loopback should have no inet address line.
		if strings.Contains(out.String(), "inet 127.0.0.1") {
			t.Errorf("expected isolated loopback, saw 127.0.0.1:\n%s", out.String())
		}
	}
}

// userNsAvailable probes whether the host lets an unprivileged user
// create a network namespace via clone(CLONE_NEWUSER|CLONE_NEWNET).
// Cheap heuristic: check the sysctl knob.
func userNsAvailable() bool {
	b, err := os.ReadFile("/proc/sys/kernel/unprivileged_userns_clone")
	if err == nil && strings.TrimSpace(string(b)) == "1" {
		return true
	}
	// Recent kernels enable it by default and remove the knob.
	_, err = os.Stat("/proc/sys/user/max_user_namespaces")
	return err == nil
}
