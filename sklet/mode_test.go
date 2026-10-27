package sklet

import "testing"

// TestChooseDeployMode: the mode decision is pure and covers every case —
// systemd available + no -foreground => detached (systemd); systemd unavailable
// + no -foreground => daemonless detach (setsid); -foreground => foreground
// regardless of systemd.
func TestChooseDeployMode(t *testing.T) {
	cases := []struct {
		name       string
		foreground bool
		systemdUp  bool
		want       Mode
	}{
		{"systemd up, no flag -> detached", false, true, ModeDetached},
		{"systemd down, no flag -> daemonless detach", false, false, ModeDetachedNoSystemd},
		{"flag forces foreground even with systemd", true, true, ModeForeground},
		{"flag forces foreground without systemd", true, false, ModeForeground},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ChooseMode(tc.foreground, tc.systemdUp); got != tc.want {
				t.Fatalf("ChooseMode(%v,%v) = %v, want %v", tc.foreground, tc.systemdUp, got, tc.want)
			}
		})
	}
}
