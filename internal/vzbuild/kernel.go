package vzbuild

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/lalternativefabrique/sklp/space/embed/meta"
)

// buildKernel compiles the guest kernel: fetch the Kata release, lift its
// embedded .config, enable the options spacenet needs on top, and build an
// arm64 Image (VZLinuxBootLoader rejects the gzipped vmlinuz distros ship).
func buildKernel(ctx context.Context, o Options) error {
	kataTar := filepath.Join(o.Work, "kata.tar.xz")
	if err := fetch(ctx, meta.KataKernelURL(o.Arch), kataTar); err != nil {
		return err
	}

	kataKernel := filepath.Join(o.Work, "kata-vmlinux")
	if err := extractKataKernel(ctx, kataTar, kataKernel); err != nil {
		return err
	}

	cfg, err := embeddedConfig(kataKernel)
	if err != nil {
		return fmt.Errorf("read kata kernel config: %w", err)
	}
	fmt.Fprintf(os.Stderr, "[vz] kata %s config: %d options\n", meta.KataVersion,
		strings.Count(string(cfg), "\n"))

	srcTar := filepath.Join(o.Work, "linux.tar.xz")
	if err := fetch(ctx, meta.KernelSourceURL(), srcTar); err != nil {
		return err
	}
	srcDir := filepath.Join(o.Work, "linux-"+meta.KernelVersion)
	if _, err := os.Stat(srcDir); err != nil {
		fmt.Fprintf(os.Stderr, "[vz] unpacking linux-%s\n", meta.KernelVersion)
		if err := run(ctx, o.Work, "tar", "xJf", srcTar); err != nil {
			return err
		}
	}

	if err := os.WriteFile(filepath.Join(srcDir, ".config"), cfg, 0o644); err != nil {
		return err
	}
	args := append([]string{}, meta.KernelExtras...)
	for _, opt := range args {
		if err := run(ctx, srcDir, "./scripts/config", "--enable", opt); err != nil {
			return fmt.Errorf("enable %s: %w", opt, err)
		}
	}
	karch := kernelArch(o.Arch)
	if err := run(ctx, srcDir, "make", "ARCH="+karch, "olddefconfig"); err != nil {
		return err
	}
	if err := verifyConfig(filepath.Join(srcDir, ".config")); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "[vz] compiling linux-%s (-j%d), this takes a while\n",
		meta.KernelVersion, o.Jobs)
	if err := run(ctx, srcDir, "make", "ARCH="+karch, "-j"+strconv.Itoa(o.Jobs), imageTarget(o.Arch)); err != nil {
		return err
	}

	built := filepath.Join(srcDir, "arch", karch, "boot", imageTarget(o.Arch))
	return install(built, filepath.Join(o.Out, "kernel"))
}

func kernelArch(goarch string) string {
	if goarch == "amd64" {
		return "x86_64"
	}
	return "arm64"
}

// arm64 boots an uncompressed Image; x86_64 has no such target and uses
// bzImage, which VZLinuxBootLoader also accepts.
func imageTarget(goarch string) string {
	if goarch == "amd64" {
		return "bzImage"
	}
	return "Image"
}

// extractKataKernel pulls the guest kernel out of the Kata release tarball.
// vmlinux.container is a symlink, so the versioned target is extracted instead.
func extractKataKernel(ctx context.Context, tarPath, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	out, err := exec.CommandContext(ctx, "tar", "tJf", tarPath).Output()
	if err != nil {
		return fmt.Errorf("list %s: %w", tarPath, err)
	}
	var member string
	for _, line := range strings.Split(string(out), "\n") {
		base := filepath.Base(line)
		if strings.HasPrefix(base, "vmlinux-") && !strings.Contains(base, "nvidia") &&
			!strings.Contains(base, "dragonball") {
			member = strings.TrimPrefix(line, "./")
			break
		}
	}
	if member == "" {
		return fmt.Errorf("no guest kernel found in %s", tarPath)
	}
	dir := filepath.Dir(tarPath)
	if err := run(ctx, dir, "tar", "xJf", filepath.Base(tarPath), "./"+member); err != nil {
		return err
	}
	return install(filepath.Join(dir, member), dst)
}

// embeddedConfig lifts the .config a kernel carries via CONFIG_IKCONFIG_PROC:
// a gzip member right after the IKCFG_ST marker.
func embeddedConfig(kernel string) ([]byte, error) {
	b, err := os.ReadFile(kernel)
	if err != nil {
		return nil, err
	}
	i := bytes.Index(b, []byte("IKCFG_ST"))
	if i < 0 {
		return nil, fmt.Errorf("kernel carries no embedded config (CONFIG_IKCONFIG missing)")
	}
	g := bytes.Index(b[i:], []byte{0x1f, 0x8b, 0x08})
	if g < 0 {
		return nil, fmt.Errorf("no gzip member after IKCFG_ST")
	}
	zr, err := gzip.NewReader(bytes.NewReader(b[i+g:]))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	cfg, err := io.ReadAll(zr)
	// The member is followed by the rest of the kernel image, so the trailing
	// bytes are expected to be garbage: keep what decompressed cleanly.
	if err != nil && len(cfg) == 0 {
		return nil, err
	}
	return cfg, nil
}

// verifyConfig fails the build if olddefconfig dropped an option the guest
// cannot boot or network without, rather than letting it surface later as a
// mount failure or an nft rule that will not load.
func verifyConfig(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	required := append([]string{
		"VIRTIO_CONSOLE", "VIRTIO_FS", "FUSE_FS", "VSOCKETS", "VIRTIO_VSOCKETS",
		"VIRTIO_BLK", "OVERLAY_FS", "USER_NS", "NET_NS", "VETH", "BRIDGE",
	}, meta.KernelExtras...)
	text := string(b)
	var missing []string
	for _, k := range required {
		if !strings.Contains(text, "\nCONFIG_"+k+"=y") {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("config is missing %s as built-in (=y): the guest would boot without them", strings.Join(missing, ", "))
	}
	return nil
}
