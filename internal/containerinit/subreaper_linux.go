//go:build linux

package containerinit

import "golang.org/x/sys/unix"

func setChildSubreaper() {
	_ = unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0)
}
