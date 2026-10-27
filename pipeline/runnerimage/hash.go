package runnerimage

import (
	"crypto/sha256"
	"encoding/hex"
)

// RootfsHash is the cache key for a built runner rootfs: a short SHA-256 of the
// RENDERED Dockerfile, not of config.yaml. Hashing the rendered recipe (rather
// than the raw YAML) captures everything that affects the produced rootfs —
// toolchain, packages, go_tools, base image, AND a change in render logic after
// an sklp upgrade — while ignoring config noise (non-runner fields, key order).
// `sklp dev` keys the rootfs dir on this; an unchanged recipe means no rebuild.
//
// 12 hex chars (48 bits) is ample for the handful of recipe variants a dev host
// builds; it mirrors the short-hash convention used elsewhere (image tags).
func RootfsHash(r Runner) (string, error) {
	df, err := renderDockerfile(r)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(df))
	return hex.EncodeToString(sum[:])[:12], nil
}

// NeedsRunnerImage reports whether a run: service must run on the BUILT runner
// rootfs (packages + go_tools baked) rather than the minimal glibc base with the
// host toolchain bind-mounted. The rule: the project declares runner.packages —
// system libs the minimal base does not ship, so they must be baked.
//
// We deliberately do NOT trigger on GITHUB_TOKEN/private-module presence (a
// bind-mounted netrc covers private fetches without baking an image), nor on a
// toolchain alone (that is already handled by the skt-cache bind-mount). No
// sentinel/opt-in field: the schema forbids a run: service from also setting an
// image, so packages are the only signal.
func NeedsRunnerImage(r Runner) bool {
	return len(r.Packages) > 0
}
