package platform

import (
	"runtime"
	"testing"
)

func TestParse(t *testing.T) {
	cases := map[string]string{
		"":              "linux/" + runtime.GOARCH,
		"linux/amd64":   "linux/amd64",
		" linux/arm64 ": "linux/arm64",
		"amd64":         "linux/amd64",
		"linux/x86_64":  "linux/amd64",
		"linux/aarch64": "linux/arm64",
	}
	for in, want := range cases {
		p, err := Parse(in)
		if err != nil {
			t.Errorf("Parse(%q): %v", in, err)
			continue
		}
		if p.String() != want {
			t.Errorf("Parse(%q) = %s, want %s", in, p, want)
		}
	}
	for _, in := range []string{"windows/amd64", "linux/riscv64", "linux/", "a/b/c"} {
		if _, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) accepted", in)
		}
	}
}

func TestPlatformQemuArch(t *testing.T) {
	if got := (Platform{OS: "linux", Arch: "amd64"}).QemuArch(); got != "x86_64" {
		t.Errorf("amd64 qemu arch = %s", got)
	}
	if got := (Platform{OS: "linux", Arch: "arm64"}).QemuArch(); got != "aarch64" {
		t.Errorf("arm64 qemu arch = %s", got)
	}
	if !Host().Native() {
		t.Error("host platform is not native")
	}
}
