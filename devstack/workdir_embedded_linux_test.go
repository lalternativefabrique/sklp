//go:build linux

package devstack

import "testing"

// containerWorkdir must join the service's repo-relative workdir onto the repo
// mount so a run: service with `workdir: services/api` runs there, not the root.
// Regression: the embedded backend used to hard-set WorkdirOverride = RepoMount,
// so `go run .` ran at /workspace and failed with "cannot find main module".
func TestContainerWorkdir(t *testing.T) {
	cases := []struct {
		mount, workdir, want string
	}{
		{"/workspace", "services/api", "/workspace/services/api"},
		{"/workspace", "", "/workspace"},
		{"/workspace", ".", "/workspace"},
		{"/workspace", "apps/transcript-api", "/workspace/apps/transcript-api"},
	}
	for _, c := range cases {
		got := containerWorkdir(DevServiceSpec{RepoMount: c.mount, Workdir: c.workdir})
		if got != c.want {
			t.Errorf("containerWorkdir(mount=%q, workdir=%q) = %q, want %q", c.mount, c.workdir, got, c.want)
		}
	}
}
