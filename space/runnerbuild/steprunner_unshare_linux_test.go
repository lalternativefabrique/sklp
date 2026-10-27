//go:build linux

package runnerbuild

import (
	"context"
	"debug/elf"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/lalternativefabrique/sklp/internal/integtest"
)

func skipUnlessUserns(t *testing.T) { integtest.SkipUnlessRootlessBuild(t) }

func TestUnshareBuildArgs_UsesHostSubuidRangeByDefault(t *testing.T) {
	args := unshareBuildArgs("true", false)
	for _, want := range []string{"--map-users=1:100000:65535", "--map-groups=1:100000:65535"} {
		if !slices.Contains(args, want) {
			t.Fatalf("args missing %s: %v", want, args)
		}
	}
}

func TestUnshareBuildArgs_NestedOmitsSecondaryRange(t *testing.T) {
	args := unshareBuildArgs("true", true)
	for _, banned := range []string{"--map-users=1:1:65535", "--map-groups=1:1:65535",
		"--map-users=1:100000:65535", "--map-groups=1:100000:65535"} {
		if slices.Contains(args, banned) {
			t.Fatalf("nested args must not contain secondary range %s: %v", banned, args)
		}
	}
	if !slices.Contains(args, "--map-root-user") {
		t.Fatalf("nested args missing --map-root-user: %v", args)
	}
}

func TestUnshareStepRunner_WithStageDropsCacheMountsWhenNested(t *testing.T) {
	base := UnshareStepRunner{CacheMounts: []CacheMount{{Source: "/a", Target: "/a"}}}
	base.NestedUserns = true
	staged := base.WithStage([]string{"PATH=/bin"}, []CacheMount{{Source: "/b", Target: "/b"}}).(UnshareStepRunner)
	if len(staged.CacheMounts) != 0 {
		t.Fatalf("cache mounts = %+v, want none", staged.CacheMounts)
	}
}

func TestUnshareStepRunner_WithStageKeepsCacheMountsByDefault(t *testing.T) {
	base := UnshareStepRunner{
		CacheMounts:  []CacheMount{{Source: "/a", Target: "/a"}},
		detectNested: func() bool { return false },
	}
	staged := base.WithStage([]string{"PATH=/bin"}, []CacheMount{{Source: "/b", Target: "/b"}}).(UnshareStepRunner)
	if len(staged.CacheMounts) != 2 {
		t.Fatalf("cache mount count = %d, want 2: %+v", len(staged.CacheMounts), staged.CacheMounts)
	}
}

// TestUnshareStepRunner_RunsInRootfs proves the real step runner executes a RUN
// inside the rootfs as uid 0: a step that writes a file and chowns it to a
// service uid (the dpkg pattern) succeeds, and the file lands in the rootfs.
// It needs a self-contained rootfs with a working /bin/sh + loader; we build one
// with `busybox --install` if busybox is present, else skip. No apt — kept
// hermetic and fast; the full apt path is exercised by the runnerbuild-demo.
func TestUnshareStepRunner_RunsInRootfs(t *testing.T) {
	skipUnlessUserns(t)
	busybox, err := exec.LookPath("busybox")
	if err != nil {
		t.Skip("busybox not installed: cannot assemble a self-contained test rootfs")
	}
	// The bare rootfs has no loader and no libc: only a STATIC busybox can
	// exec in the chroot. Debian's default busybox is dynamic and fails with
	// a misleading "/bin/sh: No such file or directory" (the missing file is
	// the ELF interpreter).
	if f, err := elf.Open(busybox); err == nil {
		interp := f.Section(".interp")
		f.Close()
		if interp != nil {
			t.Skipf("%s is dynamically linked: cannot run in a bare rootfs", busybox)
		}
	}

	rootfs := t.TempDir()
	for _, d := range []string{"bin", "proc", "tmp", "dev", "out"} {
		if err := os.MkdirAll(filepath.Join(rootfs, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A static-ish busybox gives /bin/sh + chown + echo with no loader deps
	// (or a self-contained one). Symlink the applets we use to the busybox bin.
	if err := exec.Command("cp", busybox, filepath.Join(rootfs, "bin", "busybox")).Run(); err != nil {
		t.Fatalf("copy busybox: %v", err)
	}
	for _, applet := range []string{"sh", "echo", "chown"} {
		_ = os.Symlink("busybox", filepath.Join(rootfs, "bin", applet))
	}

	steps := UnshareStepRunner{}
	if err := steps.Exec(context.Background(), rootfs, []string{
		"echo hello > /out/marker && chown 100:100 /out/marker",
	}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(rootfs, "out", "marker"))
	if err != nil {
		t.Fatalf("step output not in rootfs: %v", err)
	}
	if string(b) != "hello\n" {
		t.Fatalf("marker = %q, want %q", b, "hello\n")
	}
}
