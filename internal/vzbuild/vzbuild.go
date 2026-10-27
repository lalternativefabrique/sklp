// Package vzbuild produces the two artifacts the vz backend boots: a Linux
// guest kernel and an ext4 rootfs image.
//
// Both steps need a Linux builder with root: compiling a kernel and running
// mkfs/chroot are not things macOS can do. On macOS the build therefore runs
// through the same relay sklp already uses, which is why this package only
// assembles the plan — Run executes it locally when it is already on Linux and
// reports what to do otherwise.
package vzbuild

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/lalternativefabrique/sklp/space/embed/meta"
)

// Re-exported pins so callers (the CLI help text) need not import meta.
const (
	KataVersion   = meta.KataVersion
	KernelVersion = meta.KernelVersion
	GuestBase     = meta.GuestBase
)

var KernelExtras = meta.KernelExtras

func Artifacts() []meta.VMArtifact { return meta.VMArtifacts }

// CacheDir is where the vz backend looks for its artifacts. It mirrors the
// lookup in internal/limarelay so both sides agree without a shared import.
func CacheDir() string {
	if d := os.Getenv("SKLP_VZ_DIR"); d != "" {
		return d
	}
	// Honour XDG_CACHE_HOME like the rest of the sklp caches, so a test or a
	// sandboxed run can point the artifacts somewhere else.
	if d := os.Getenv("XDG_CACHE_HOME"); d != "" {
		return filepath.Join(d, "sklp", "vz")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".sklp-vz"
	}
	return filepath.Join(home, ".cache", "sklp", "vz")
}

// Options controls a build run.
type Options struct {
	Out        string // destination dir (default CacheDir())
	Work       string // scratch dir for sources and the staged rootfs
	Arch       string // guest arch: arm64 (amd64 untested)
	KernelOnly bool
	RootfsOnly bool
	Jobs       int
	SklpBin    string // linux sklp to bake in; built from source when empty
	SktBin     string // linux skt to bake in; built from source when empty
	AgentBin   string // linux sklp-vzagent to bake in as /init; built when empty
}

func ParseFlags(argv []string) (Options, error) {
	var o Options
	fs := flag.NewFlagSet("vz build", flag.ContinueOnError)
	fs.StringVar(&o.Out, "out", "", "output directory (default: the vz cache)")
	fs.StringVar(&o.Work, "work", "", "scratch directory (default: <out>/build)")
	fs.StringVar(&o.Arch, "arch", runtime.GOARCH, "guest architecture")
	fs.BoolVar(&o.KernelOnly, "kernel-only", false, "build only the kernel")
	fs.BoolVar(&o.RootfsOnly, "rootfs-only", false, "build only the rootfs image")
	fs.IntVar(&o.Jobs, "jobs", 0, "parallel compile jobs (0 = NumCPU)")
	fs.StringVar(&o.SklpBin, "sklp-bin", "", "linux sklp binary to bake into the rootfs")
	fs.StringVar(&o.SktBin, "skt-bin", "", "linux skt binary to bake into the rootfs")
	fs.StringVar(&o.AgentBin, "agent-bin", "", "linux sklp-vzagent binary to bake in as the guest init")
	if err := fs.Parse(argv); err != nil {
		return o, err
	}
	if o.KernelOnly && o.RootfsOnly {
		return o, fmt.Errorf("-kernel-only and -rootfs-only are mutually exclusive")
	}
	if o.Out == "" {
		o.Out = CacheDir()
	}
	if o.Work == "" {
		o.Work = filepath.Join(o.Out, "build")
	}
	if o.Jobs == 0 {
		o.Jobs = runtime.NumCPU()
	}
	return o, nil
}

// Run builds the requested artifacts.
func Run(ctx context.Context, o Options) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("the guest artifacts must be built on Linux (compiling a kernel and running mkfs need it).\n"+
			"On macOS, `sklp macos build -rootfs-only` relays the build into the vz VM;\n"+
			"otherwise build on a Linux box and copy kernel + rootfs.img into %s",
			CacheDir())
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("building the rootfs needs root (chroot + mkfs.ext4); re-run under sudo")
	}
	if err := os.MkdirAll(o.Out, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(o.Work, 0o755); err != nil {
		return err
	}
	if !o.RootfsOnly {
		if err := buildKernel(ctx, o); err != nil {
			return fmt.Errorf("kernel: %w", err)
		}
	}
	if !o.KernelOnly {
		if err := buildRootfs(ctx, o); err != nil {
			return fmt.Errorf("rootfs: %w", err)
		}
	}
	return nil
}
