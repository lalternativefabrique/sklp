//go:build linux

package imagebuild

import (
	"slices"
	"testing"
)

func TestCpArgv(t *testing.T) {
	direct := []string{"cp", "-a", "src/.", "dst"}
	mapped := []string{"unshare", "--user", "--map-root-user",
		"--map-users=1:100000:65535", "--map-groups=1:100000:65535",
		"--fork", "cp", "-a", "src/.", "dst"}

	cases := []struct {
		name   string
		nested bool
		uid    int
		want   []string
	}{
		{"rootless host maps the subuid range", false, 1000, mapped},
		{"real root copies owners verbatim", false, 0, direct},
		{"nested runner copies directly", true, 1000, direct},
		{"nested root copies directly", true, 0, direct},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cpArgv(tc.nested, tc.uid, []string{"src/.", "dst"})
			if !slices.Equal(got, tc.want) {
				t.Fatalf("cpArgv = %q, want %q", got, tc.want)
			}
		})
	}
}
