package imagebuild

import (
	"fmt"
	"os"
	"strings"

	"github.com/lalternativefabrique/sklp/space/platform"
)

const binfmtDir = "/proc/sys/fs/binfmt_misc"

// CheckEmulation reports whether RUN steps of a build for target can execute
// on this host: natively, or through a binfmt_misc qemu-user handler. The
// handler needs the F flag so it stays usable inside a container whose rootfs
// does not carry the interpreter.
func CheckEmulation(target platform.Platform) error {
	if target.IsZero() || target.Native() {
		return nil
	}
	host := platform.Host()
	hint := fmt.Sprintf("building %s on a %s host needs binfmt emulation: install qemu-user-static with binfmt registration (Arch: qemu-user-static-binfmt, Debian: qemu-user-static binfmt-support), or build on a %s machine",
		target, host, target.Arch)
	b, err := os.ReadFile(binfmtDir + "/qemu-" + target.QemuArch())
	if err != nil {
		return fmt.Errorf("%s (no %s handler registered)", hint, target.QemuArch())
	}
	status := string(b)
	if !strings.HasPrefix(status, "enabled") {
		return fmt.Errorf("%s (handler qemu-%s is disabled)", hint, target.QemuArch())
	}
	for _, line := range strings.Split(status, "\n") {
		if flags, ok := strings.CutPrefix(line, "flags: "); ok && !strings.Contains(flags, "F") {
			return fmt.Errorf("%s (handler qemu-%s lacks the F flag, so it cannot run inside the build container)", hint, target.QemuArch())
		}
	}
	return nil
}
