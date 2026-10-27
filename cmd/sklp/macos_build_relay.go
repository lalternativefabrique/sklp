package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/lalternativefabrique/sklp/internal/limarelay"
	"github.com/lalternativefabrique/sklp/internal/vzbuild"
)

// vzBuildViaVM runs the rootfs build INSIDE the vz VM: the guest is the Linux
// builder the build needs (chroot, mkfs.ext4), and the shared home carries the
// binaries in and the image out. The Go compilation happens on the Mac — the
// guest has no toolchain — and the relayed argv executes the freshly built
// linux sklp from the share, so the build always runs today's code, never
// whatever the guest image was baked with.
func vzBuildViaVM(ctx context.Context, o vzbuild.Options) int {
	if !o.RootfsOnly {
		fmt.Fprintln(os.Stderr, "sklp macos build: the kernel build needs a Linux box with a compiler;\n"+
			"on macOS only `-rootfs-only` is supported (it runs inside the vz VM).")
		return 2
	}

	staging := filepath.Join(vzbuild.CacheDir(), "relay")
	outNext := filepath.Join(staging, "out")
	if err := os.MkdirAll(outNext, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "sklp macos build: %v\n", err)
		return 1
	}

	sklpBin := filepath.Join(staging, "sklp-linux")
	agentBin := filepath.Join(staging, "sklp-vzagent")
	for _, b := range []struct {
		out, pkg string
		tags     string
	}{
		{sklpBin, "./apps/sklp/cmd/sklp", "embedded_runtime"},
		{agentBin, "./apps/sklp/cmd/sklp-vzagent", ""},
	} {
		args := []string{"build", "-trimpath", "-ldflags=-s -w"}
		if b.tags != "" {
			args = append(args, "-tags", b.tags)
		}
		args = append(args, "-o", b.out, b.pkg)
		cmd := exec.CommandContext(ctx, "go", args...)
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+o.Arch)
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		fmt.Fprintf(os.Stderr, "[vz] cross-building %s for linux/%s\n", filepath.Base(b.out), o.Arch)
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "sklp macos build: cross-build %s: %v\n", b.pkg, err)
			return 1
		}
	}

	// skt is a Rust crate this repo cannot cross-build; without it the guest
	// image loses the toolchain installs (`tools:` pipelines, the dev stack's
	// gitleaks). When the flag is not given, fetch the published linux skt
	// from the dist bucket (cached under the vz dir).
	if o.SktBin == "" {
		skt, err := limarelay.FetchSktLinux(o.Arch)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[vz] no skt binary (-skt-bin, dist fetch failed: %v) — the built guest will fail toolchain installs\n", err)
		} else {
			o.SktBin = skt
		}
	}

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "sklp macos build: %v\n", err)
		return 1
	}
	argv := []string{sklpBin, "macos", "build",
		"-rootfs-only",
		"-arch", o.Arch,
		"-sklp-bin", sklpBin,
		"-agent-bin", agentBin,
		"-out", outNext,
		"-work", "/var/tmp/sklp-vzbuild",
	}
	if o.SktBin != "" {
		argv = append(argv, "-skt-bin", o.SktBin)
	}
	if err := limarelay.RelayCommand(cwd, argv); err != nil {
		fmt.Fprintf(os.Stderr, "sklp macos build: %v\n", err)
		return 1
	}

	// The built image sits on the share; the live VM still boots the old one.
	// Stop the VM (this takes its spaces down, like `sklp macos down`) and
	// move the image into place so the next boot picks it up.
	img := filepath.Join(outNext, "rootfs.img")
	if _, err := os.Stat(img); err != nil {
		fmt.Fprintf(os.Stderr, "sklp macos build: guest reported success but %s is missing\n", img)
		return 1
	}
	if macosVMStop != nil {
		if stopped, err := macosVMStop(); err != nil {
			fmt.Fprintf(os.Stderr, "sklp macos build: image built at %s but the VM could not be stopped: %v\n"+
				"run `sklp macos down`, then move it over %s yourself\n",
				img, err, filepath.Join(vzbuild.CacheDir(), "rootfs.img"))
			return 1
		} else if stopped {
			fmt.Fprintln(os.Stderr, "[vz] VM stopped (its spaces went down with it)")
		}
	}
	dst := filepath.Join(vzbuild.CacheDir(), "rootfs.img")
	if err := os.Rename(img, dst); err != nil {
		fmt.Fprintf(os.Stderr, "sklp macos build: move image into place: %v\n", err)
		return 1
	}
	fmt.Printf("rootfs rebuilt: %s — the next `sklp dev` (or `sklp macos up`) boots it\n", dst)
	return 0
}

// vzBuildIsRelayed reports whether this invocation should build through the VM
// instead of locally: macOS has no chroot/mkfs.ext4, the vz guest does.
func vzBuildIsRelayed() bool { return runtime.GOOS == "darwin" }
