package meta

import "github.com/lalternativefabrique/sklp/space/host"

// The vz backend boots a Linux VM on Virtualization.framework instead of
// shelling out to Lima. Unlike runc/slirp4netns the artifacts it needs are far
// too large to //go:embed (a kernel plus a rootfs image), so they are built or
// downloaded into the cache and pinned by the descriptors below.
const (
	// KataVersion is the Kata Containers release whose guest-kernel config we
	// start from: it already builds in virtiofs, vsock, virtio-console,
	// overlayfs, userns and cgroups. It is also the kernel Apple's
	// containerization project fetches by default.
	KataVersion = "3.17.0"

	// KernelVersion is the upstream Linux tree the guest kernel is built from.
	// It matches the Kata kernel so the extracted config applies cleanly.
	KernelVersion = "6.12.28"

	// GuestBase is the OCI image the guest rootfs is built from. It is
	// deliberately the same base sklp builds its runner rootfs from, so the VM
	// presents the Linux the CI runner and deploy nodes present.
	GuestBase = host.BaseImage
)

// KernelExtras are the options added on top of the Kata config. Kata does not
// ship them because it does networking at the VM level, but sklp's spacenet
// needs them inside the guest:
//
//   - TUN            slirp4netns creates the egress tap on /dev/net/tun
//   - NF_TABLES…     egress.go programs its masquerade rule with `nft`, which
//     speaks NETLINK_NETFILTER; the legacy IP_NF_* options Kata
//     does ship are never used (sklp calls iptables nowhere)
//   - NFT_MASQ       the nftables masquerade expression itself. Distinct from
//     NF_NAT_MASQUERADE (the NAT engine): without it `nft`
//     creates the table and chain, then rejects the rule with
//     "Could not process rule: No such file or directory"
//   - NFT_REDIR      spacemanager's gateway redirect (:80→:9080)
var KernelExtras = []string{
	"TUN",
	"NF_TABLES",
	"NF_TABLES_IPV4",
	"NFT_NAT",
	"NFT_MASQ",
	"NFT_REDIR",
	"NFT_CT",
	"NFT_REJECT",
	"NF_NAT_MASQUERADE",
	"NF_CONNTRACK",
	"NFT_COMPAT",
}

// VMArtifact describes one file the vz backend needs in its cache.
type VMArtifact struct {
	Name   string // file name under the vz cache dir
	SHA256 string // empty until a published build is pinned
}

// VMArtifacts are the files `sklp vz build` produces. Their sha256 stay empty
// while the artifacts are built locally; pin them once a published build is
// served from the dist bucket.
var VMArtifacts = []VMArtifact{
	{Name: "kernel"},
	{Name: "rootfs.img"},
}

// KataKernelURL is the Kata release tarball the base kernel config is
// extracted from (the kernel binary embeds its own .config via IKCFG).
func KataKernelURL(arch string) string {
	return "https://github.com/kata-containers/kata-containers/releases/download/" +
		KataVersion + "/kata-static-" + KataVersion + "-" + arch + ".tar.xz"
}

// KernelSourceURL is the upstream tarball the guest kernel is compiled from.
func KernelSourceURL() string {
	return "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-" + KernelVersion + ".tar.xz"
}
