//go:build !linux

package devstack

import "time"

// Non-Linux stub: PR_SET_CHILD_SUBREAPER is Linux-specific. On other
// platforms proc.stop() falls back to its pgrep child-tree sweep, which is
// the behaviour that shipped before the subreaper was added. sklp's
// supervisor is Linux-only in practice (laptops + CI runners), so this
// stub exists purely to keep cross-compilation green.

func installSubreaper() {}

func reapOrphans(time.Duration) {}
