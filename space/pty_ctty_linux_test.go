//go:build linux

package space

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

const ptyChildEnv = "SKLP_TEST_PTY_CTTY_CHILD"

// A session leader without a controlling terminal — the node agent under
// systemd — must not acquire one by opening the pts for an exec: the hang-up
// at the end of the exec would SIGHUP it. The check runs in a child started
// with Setsid so the test process's own terminal cannot mask the result.
func TestOpenPTYSlaveDoesNotBecomeControllingTerminal(t *testing.T) {
	if os.Getenv(ptyChildEnv) == "1" {
		master, name, err := openPTY()
		if err != nil {
			t.Fatal(err)
		}
		defer master.Close()
		pts, err := openPTYSlave(name)
		if err != nil {
			t.Fatal(err)
		}
		defer pts.Close()
		if _, err := os.Stdout.WriteString(controllingTTY(t)); err != nil {
			t.Fatal(err)
		}
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestOpenPTYSlaveDoesNotBecomeControllingTerminal$", "-test.v=false")
	cmd.Env = append(os.Environ(), ptyChildEnv+"=1")
	cmd.SysProcAttr = &unix.SysProcAttr{Setsid: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	first := strings.SplitN(string(out), "\n", 2)[0]
	if first != "0" {
		t.Fatalf("child acquired a controlling terminal (tty_nr=%q); the agent would die of SIGHUP when the exec ends", first)
	}
}

// controllingTTY returns field 7 (tty_nr) of /proc/self/stat: "0" means none.
func controllingTTY(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		t.Fatal(err)
	}
	rest := data[strings.LastIndexByte(string(data), ')')+2:]
	fields := strings.Fields(string(rest))
	return fields[4] + "\n"
}
