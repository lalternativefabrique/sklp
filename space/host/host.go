// Package host is the one definition of what a machine needs to run a sklp
// Space: the base image, the packages sklp shells out to, the subuid ranges,
// and a preflight that names what is missing. The vz guest, the runner image,
// the CI Space rootfs and `sklp host check` all read it; none keeps its own
// list.
package host

import (
	"os"
	"path/filepath"
	"slices"
)

// BaseImage is the OCI image every sklp host rootfs starts from.
const BaseImage = "debian:trixie-slim"

// Packages are what sklp needs beyond BaseImage. It shells out to each by name:
// `nft` (egress NAT), `unshare` from util-linux (BusyBox's applet has no
// --map-users), newuidmap/newgidmap from uidmap (the subuid range map), `git`
// (the impact diff), `ip` (tap/bridge plumbing), curl and xz for tool fetches,
// udhcpc for the vz guest's virtio NIC, e2fsprogs so the vz guest can
// mkfs.ext4 its own successor rootfs image (`sklp macos build` relays the
// rootfs build into the running VM).
var Packages = []string{
	"nftables",
	"iproute2",
	"uidmap",
	"util-linux",
	"git",
	"ca-certificates",
	"curl",
	"xz-utils",
	"udhcpc",
	"e2fsprogs",
}

// WithPackages returns Packages followed by extra, without duplicates, so a
// recipe's own packages layer on top of the host's instead of replacing them.
func WithPackages(extra []string) []string {
	out := slices.Clone(Packages)
	for _, p := range extra {
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// Subids is the /etc/subuid and /etc/subgid content a host rootfs ships: a
// 65536-wide range for root (uid 0 inside a Space or a pod) and for the sklp
// service user.
const Subids = "root:100000:65536\nsklp:200000:65536\n"

// WriteSubids installs Subids into a staged rootfs.
func WriteSubids(root string) error {
	for _, f := range []string{"etc/subuid", "etc/subgid"} {
		if err := os.WriteFile(filepath.Join(root, f), []byte(Subids), 0o644); err != nil {
			return err
		}
	}
	return nil
}
