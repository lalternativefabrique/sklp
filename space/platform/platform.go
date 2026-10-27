package platform

import (
	"fmt"
	"runtime"
	"strings"
)

// Platform is the OCI os/arch pair an image is built for.
type Platform struct {
	OS   string
	Arch string
}

var knownArches = map[string]bool{"amd64": true, "arm64": true}

func Host() Platform {
	return Platform{OS: "linux", Arch: runtime.GOARCH}
}

// Parse accepts "linux/amd64", "linux/arm64" and the bare arch.
// An empty string is the host platform.
func Parse(s string) (Platform, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Host(), nil
	}
	os, arch, ok := strings.Cut(s, "/")
	if !ok {
		os, arch = "linux", s
	}
	switch arch {
	case "x86_64":
		arch = "amd64"
	case "aarch64":
		arch = "arm64"
	}
	if os != "linux" || !knownArches[arch] {
		return Platform{}, fmt.Errorf("unsupported platform %q (linux/amd64 or linux/arm64)", s)
	}
	return Platform{OS: os, Arch: arch}, nil
}

func (p Platform) String() string { return p.OS + "/" + p.Arch }

func (p Platform) IsZero() bool { return p.OS == "" && p.Arch == "" }

func (p Platform) Native() bool { return p == Host() }

// QemuArch is the binfmt_misc handler name qemu-user registers for this arch.
func (p Platform) QemuArch() string {
	switch p.Arch {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	}
	return p.Arch
}
