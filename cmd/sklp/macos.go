package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/internal/vzbuild"
)

// Seams filled in by the darwin/arm64/cgo build; nil elsewhere, so the verbs
// report the backend as unavailable rather than pretending.
var (
	macosDaemonRun func() int
	macosVMRunning func() bool
	macosVMStop    func() (bool, error)
	macosVMStart   func() error
)

func macosCommand() cli.Command {
	return cli.Command{
		Name:  "macos",
		Short: "Manage the Linux VM sklp runs on macOS (Virtualization.framework)",
		Long: `sklp macos — the Linux VM sklp relays into on this Mac.

The vz backend runs the Linux guest on Apple's Virtualization.framework
instead of shelling out to Lima, so macOS needs no VM manager installed. It
needs two artifacts in its cache:

  kernel      Linux ` + vzbuild.KernelVersion + ` with the Kata guest config plus the
              netfilter/tun options spacenet needs (a stock distro kernel
              ships those as modules and will not boot here)
  rootfs.img  an ext4 image built from ` + vzbuild.GuestBase + `, carrying sklp,
              skt and the tools sklp shells out to

This backend is the default on Apple silicon; SKLP_VM_PROVIDER=lima|orbstack
picks another. The artifacts live under ~/.cache/sklp/vz (SKLP_VZ_DIR).`,
		Sub: []cli.Command{
			{
				Name:  "build",
				Short: "Build the guest kernel and rootfs image",
				Long: `Builds both artifacts from pinned sources.

The kernel is compiled from upstream ` + vzbuild.KernelVersion + ` with the config
extracted from the Kata ` + vzbuild.KataVersion + ` guest kernel; the rootfs is
assembled from the ` + vzbuild.GuestBase + ` OCI layer. Both steps need a Linux
builder: this runs them wherever sklp is already relaying to.`,
				RawArgs: true,
				Run: func(ctx context.Context, args cli.Args) int {
					return vzBuild(ctx, args.Passthrough())
				},
			},
			{
				Name:  "up",
				Short: "Start the VM now instead of on the next command",
				Run: func(_ context.Context, _ cli.Args) int {
					if macosVMStart == nil {
						fmt.Fprintln(os.Stderr, "sklp macos up: this build has no native VM backend")
						return 1
					}
					if macosVMRunning != nil && macosVMRunning() {
						fmt.Println("already running")
						return 0
					}
					if err := macosVMStart(); err != nil {
						fmt.Fprintf(os.Stderr, "sklp macos up: %v\n", err)
						return 1
					}
					fmt.Println("started")
					return 0
				},
			},
			{
				Name:  "down",
				Short: "Stop the VM, taking every space running in it with it",
				Run: func(_ context.Context, _ cli.Args) int {
					if macosVMStop == nil {
						fmt.Fprintln(os.Stderr, "sklp macos down: this build has no native VM backend")
						return 1
					}
					stopped, err := macosVMStop()
					if err != nil {
						fmt.Fprintf(os.Stderr, "sklp macos down: %v\n", err)
						return 1
					}
					if !stopped {
						fmt.Println("not running")
						return 0
					}
					fmt.Println("stopped")
					return 0
				},
			},
			{
				Name:  "status",
				Short: "Show which artifacts are present and their versions",
				Run: func(ctx context.Context, args cli.Args) int {
					return vzStatus()
				},
			},
		},
	}
}

func vzBuild(ctx context.Context, argv []string) int {
	opts, err := vzbuild.ParseFlags(argv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sklp macos build: %v\n", err)
		return 2
	}
	if vzBuildIsRelayed() {
		return vzBuildViaVM(ctx, opts)
	}
	if err := vzbuild.Run(ctx, opts); err != nil {
		fmt.Fprintf(os.Stderr, "sklp macos build: %v\n", err)
		return 1
	}
	return 0
}

func vzStatus() int {
	if macosVMRunning != nil {
		state := "stopped"
		if macosVMRunning() {
			state = "running"
		}
		fmt.Printf("vm:       %s\n", state)
	}
	dir := vzbuild.CacheDir()
	fmt.Printf("artifacts: %s\n", dir)
	fmt.Printf("  kernel  linux-%s (kata %s config + %d extra options)\n",
		vzbuild.KernelVersion, vzbuild.KataVersion, len(vzbuild.KernelExtras))
	fmt.Printf("  rootfs  %s\n", vzbuild.GuestBase)
	fmt.Println()

	missing := 0
	for _, a := range vzbuild.Artifacts() {
		path := filepath.Join(dir, a.Name)
		fi, err := os.Stat(path)
		if err != nil {
			fmt.Printf("  %-12s missing\n", a.Name)
			missing++
			continue
		}
		fmt.Printf("  %-12s %6.1f MB  %s\n", a.Name,
			float64(fi.Size())/(1024*1024), fi.ModTime().Format("2006-01-02 15:04"))
	}
	if missing > 0 {
		fmt.Fprintf(os.Stderr, "\n%d artifact(s) missing — run `sklp macos build`\n", missing)
		return 1
	}
	return 0
}
