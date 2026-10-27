package control

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/spacemanager"
	"golang.org/x/sys/unix"
)

// openClientPTY allocates a real PTY pair and returns the master file and the
// opened slave file. The slave stands in for the user's terminal (the client's
// stdin/stdout under `sklp deploy exec web`); the master is what the test reads
// to observe what the terminal would have displayed. This reproduces the REAL
// TTY condition that a bytes.Buffer stdout cannot: the client's stdout is a tty.
func openClientPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatalf("open ptmx: %v", err)
	}
	n, err := unix.IoctlGetInt(int(m.Fd()), unix.TIOCGPTN)
	if err != nil {
		m.Close()
		t.Fatalf("TIOCGPTN: %v", err)
	}
	if err := unix.IoctlSetPointerInt(int(m.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		m.Close()
		t.Fatalf("TIOCSPTLCK: %v", err)
	}
	s, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		m.Close()
		t.Fatalf("open pts: %v", err)
	}
	t.Cleanup(func() { s.Close(); m.Close() })
	return m, s
}

// TestExecTTYRelaysOutputToRealTerminal drives the full interactive chain with a
// REAL PTY as the client's stdin/stdout (what the user actually has under
// `sklp deploy exec web -- echo HELLO`), not a bytes.Buffer. This is the
// condition the buffer-based test cannot reproduce: the client's stdout is a
// terminal. The container's output MUST appear on the client's terminal master.
func TestExecTTYRelaysOutputToRealTerminal(t *testing.T) {
	runc := fakeRuncExecBin(t)
	sup := &fakeSupervisor{
		services: []spacemanager.ServiceSnapshot{{Name: "web"}},
		execTargets: map[string]spacemanager.ExecTarget{
			"web": {RuncBin: runc, RuncRoot: t.TempDir(), ContainerID: "web-0"},
		},
	}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	master, slave := openClientPTY(t)

	const marker = "HELLO-REAL-TTY"
	ctx, cctx := context.WithTimeout(context.Background(), 8*time.Second)
	defer cctx()

	done := make(chan error, 1)
	go func() {
		// stdin AND stdout are the SAME terminal slave, exactly as the CLI wires
		// os.Stdin/os.Stdout when both are a tty.
		_, err := c.Exec(ctx, "web", []string{"sh", "-c", "echo " + marker}, true, 24, 80, slave, slave, slave)
		done <- err
	}()

	// Read what the terminal received off the master.
	out := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		var acc []byte
		for {
			master.SetReadDeadline(time.Now().Add(6 * time.Second))
			n, err := master.Read(buf)
			if n > 0 {
				acc = append(acc, buf[:n]...)
				if strings.Contains(string(acc), marker) {
					out <- string(acc)
					return
				}
			}
			if err != nil {
				out <- string(acc)
				return
			}
		}
	}()

	select {
	case got := <-out:
		if !strings.Contains(got, marker) {
			t.Fatalf("real-tty exec output not relayed to terminal: got %q, missing %q", got, marker)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("real-tty exec produced no output on the terminal within deadline")
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		// Exec may still be draining; not fatal for the output assertion.
	}
}

// TestExecTTYStartFailureSurfacedToClient is the regression for the SILENT
// failure path: when `runc exec -t` cannot even START (the runc binary is
// missing / unexecutable), RuncExecTTY returns an error having written ZERO
// bytes to the PTY. The server has already sent the 101 and cannot fall back to
// an HTTP error, so without an explicit relay the session is mute and returns
// instantly — exactly the user-visible symptom. The server must write the cause
// onto the hijacked stream so the client's terminal shows WHY it is empty.
//
// (A real `runc exec -t` that fails on the container console DOES leak its
// stderr through the PTY; this test isolates the harder case where nothing
// reaches the PTY at all, which only the server-side relay covers.)
func TestExecTTYStartFailureSurfacedToClient(t *testing.T) {
	// A runc path that does not exist => exec.Cmd.Start fails, RuncExecTTY returns
	// an error with no PTY output. The error text carries the missing path.
	missing := filepath.Join(t.TempDir(), "runc-does-not-exist")
	sup := &fakeSupervisor{
		services: []spacemanager.ServiceSnapshot{{Name: "web"}},
		execTargets: map[string]spacemanager.ExecTarget{
			"web": {RuncBin: missing, RuncRoot: t.TempDir(), ContainerID: "web-0"},
		},
	}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	ctx, cctx := context.WithTimeout(context.Background(), 8*time.Second)
	defer cctx()

	var out strings.Builder
	_, err := c.Exec(ctx, "web", []string{"sh"}, true, 24, 80, strings.NewReader(""), &out, &out)
	if err != nil {
		t.Fatalf("exec tty (failure path) returned transport error: %v", err)
	}
	if !strings.Contains(out.String(), "sklp exec:") {
		t.Fatalf("exec -t start failure not surfaced to client: got %q, want it to contain the relayed cause", out.String())
	}
}
