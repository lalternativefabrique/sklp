//go:build !linux

package limarelay

import (
	"os"
	"slices"
	"testing"
)

func TestGuestCommandForwardsSetVars(t *testing.T) {
	t.Setenv("SKLP_SECRETS_PASSPHRASE", "s3cr3t")
	got := guestCommand([]string{"run", "publish-cli"})
	want := []string{"env", "SKLP_SECRETS_PASSPHRASE=s3cr3t", "sklp", "run", "publish-cli"}
	if !slices.Equal(got, want) {
		t.Fatalf("guestCommand = %v, want %v", got, want)
	}
}

// With nothing to carry the command must stay a bare `sklp …`: an empty `env`
// prefix would still work but hides the real command in ps output.
func TestGuestCommandWithoutVars(t *testing.T) {
	for _, k := range forwardedEnv {
		t.Setenv(k, "") // registers the restore, then clear it outright
		if err := os.Unsetenv(k); err != nil {
			t.Fatalf("unset %s: %v", k, err)
		}
	}
	got := guestCommand([]string{"space", "ls"})
	want := []string{"sklp", "space", "ls"}
	if !slices.Equal(got, want) {
		t.Fatalf("guestCommand = %v, want %v", got, want)
	}
}
