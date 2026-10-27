package vzbuild

import (
	"slices"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/space/embed/meta"
	"github.com/lalternativefabrique/sklp/space/host"
)

func TestParseFlagsDefaults(t *testing.T) {
	o, err := ParseFlags(nil)
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if o.Out == "" {
		t.Error("Out should default to the vz cache")
	}
	if !strings.HasPrefix(o.Work, o.Out) {
		t.Errorf("Work %q should default under Out %q", o.Work, o.Out)
	}
	if o.Jobs < 1 {
		t.Errorf("Jobs = %d, want >= 1", o.Jobs)
	}
}

func TestParseFlagsRejectsBothOnlyFlags(t *testing.T) {
	if _, err := ParseFlags([]string{"-kernel-only", "-rootfs-only"}); err == nil {
		t.Fatal("-kernel-only with -rootfs-only must be rejected: they select disjoint work")
	}
}

func TestCacheDirHonoursOverride(t *testing.T) {
	t.Setenv("SKLP_VZ_DIR", "/tmp/vz-override")
	if got := CacheDir(); got != "/tmp/vz-override" {
		t.Errorf("CacheDir() = %q, want the SKLP_VZ_DIR override", got)
	}
}

func TestKernelArch(t *testing.T) {
	for goarch, want := range map[string]string{"arm64": "arm64", "amd64": "x86_64"} {
		if got := kernelArch(goarch); got != want {
			t.Errorf("kernelArch(%q) = %q, want %q", goarch, got, want)
		}
	}
}

// arm64 must boot an uncompressed Image: VZLinuxBootLoader rejects the gzipped
// vmlinuz distros ship, failing with a bare "Internal Virtualization error".
func TestImageTarget(t *testing.T) {
	if got := imageTarget("arm64"); got != "Image" {
		t.Errorf("imageTarget(arm64) = %q, want Image", got)
	}
	if got := imageTarget("amd64"); got != "bzImage" {
		t.Errorf("imageTarget(amd64) = %q, want bzImage", got)
	}
}

// NFT_MASQ is the nftables masquerade expression and is distinct from
// NF_NAT_MASQUERADE (the NAT engine iptables uses). Without it `nft` accepts
// the table and chain, then rejects the rule egress.go programs.
func TestKernelExtrasCoverEgress(t *testing.T) {
	have := map[string]bool{}
	for _, k := range meta.KernelExtras {
		have[k] = true
	}
	for _, want := range []string{"TUN", "NF_TABLES", "NFT_NAT", "NFT_MASQ"} {
		if !have[want] {
			t.Errorf("KernelExtras is missing %s: container egress would not work", want)
		}
	}
}

// sklp shells out to each of these by name, so a base image that lacks one
// fails at run time rather than at build time.
func TestGuestPackagesCoverShellOuts(t *testing.T) {
	have := map[string]bool{}
	for _, p := range host.Packages {
		have[p] = true
	}
	for _, want := range []string{"nftables", "util-linux", "git", "uidmap"} {
		if !have[want] {
			t.Errorf("GuestPackages is missing %s", want)
		}
	}
}

func TestKataKernelURLIsPinned(t *testing.T) {
	u := meta.KataKernelURL("arm64")
	if !strings.Contains(u, meta.KataVersion) {
		t.Errorf("KataKernelURL() = %q, want it pinned to %s", u, meta.KataVersion)
	}
	if strings.Contains(u, "latest") {
		t.Error("KataKernelURL() must not float on latest")
	}
}

func TestGuestShipsQemuUser(t *testing.T) {
	got := host.WithPackages(guestExtraPackages)
	for _, want := range []string{"qemu-user-static", "binfmt-support", "nftables"} {
		if !slices.Contains(got, want) {
			t.Errorf("guest packages missing %s", want)
		}
	}
}
