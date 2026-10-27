package control

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/spacemanager"
)

// fakeRuncExecBin writes a stand-in for the runc binary: it drops the leading
// `--root <root> exec [-t] <id>` args and execs the rest as the in-container
// command on the host, so the exec path is exercised without a real container.
func fakeRuncExecBin(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runc")
	script := `#!/bin/sh
shift            # --root
shift            # <root>
shift            # exec
if [ "$1" = "-t" ]; then shift; fi
shift            # <id>
exec "$@"
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestExecTTYOneShotRelaysOutputEndToEnd drives the FULL interactive chain
// (client execTTY -> raw UDS -> server hijack -> RuncExecTTY -> PTY) for a
// one-shot command that prints then exits. It is the regression for the
// "TTY exec shows no output" bug: the server closes the hijacked conn the instant
// the exec returns, so unless the PTY output is fully drained first, a fast
// command's output is lost. The client's stdout writer must receive the marker.
func TestExecTTYOneShotRelaysOutputEndToEnd(t *testing.T) {
	runc := fakeRuncExecBin(t)
	sup := &fakeSupervisor{
		services: []spacemanager.ServiceSnapshot{{Name: "web"}},
		execTargets: map[string]spacemanager.ExecTarget{
			"web": {RuncBin: runc, RuncRoot: t.TempDir(), ContainerID: "web-0"},
		},
	}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	ctx, cctx := context.WithTimeout(context.Background(), 8*time.Second)
	defer cctx()

	// Emit a payload larger than the socket/PTY buffers so the relay is provably
	// still in flight when the exec'd process exits: without the drain wait the
	// server closes the hijacked conn mid-copy and the tail is truncated. The
	// trailing marker is the last bytes written, so a truncated stream loses it.
	const lines = 4000
	const endMarker = "TTY-E2E-END-MARKER"
	script := "i=0; while [ $i -lt " + strconv.Itoa(lines) + " ]; do echo padding-line-$i; i=$((i+1)); done; echo " + endMarker

	var out bytes.Buffer
	// Empty stdin (one-shot, no keystrokes) like `exec web -- ls /`.
	code, err := c.Exec(ctx, "web", []string{"sh", "-c", script}, true, 24, 80, strings.NewReader(""), &out, &out)
	if err != nil {
		t.Fatalf("exec tty: %v", err)
	}
	_ = code // tty path reports 0; exit code is not carried back over the raw stream
	if !strings.Contains(out.String(), endMarker) {
		t.Fatalf("tty one-shot output truncated end-to-end: got %d bytes, missing trailing %q", out.Len(), endMarker)
	}
}

// TestExecTTYInteractiveStaysOpenAndStreams proves the fix does NOT regress the
// long-lived interactive case (`sklp deploy exec web sh`): with stdin held open,
// an interactive shell streams output mid-session in reaction to typed commands,
// and the session ends cleanly only when the user asks to exit. The decoupled
// input copy must keep the output flowing while stdin is open, then let the
// session close once the shell exits.
func TestExecTTYInteractiveStaysOpenAndStreams(t *testing.T) {
	runc := fakeRuncExecBin(t)
	sup := &fakeSupervisor{
		services: []spacemanager.ServiceSnapshot{{Name: "web"}},
		execTargets: map[string]spacemanager.ExecTarget{
			"web": {RuncBin: runc, RuncRoot: t.TempDir(), ContainerID: "web-0"},
		},
	}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	ctx, cctx := context.WithTimeout(context.Background(), 8*time.Second)
	defer cctx()

	// A pipe feeds the shell's stdin: type a command (streams output mid-session),
	// then type `exit` (the shell terminates -> server closes the session).
	pr, pw := io.Pipe()
	var out lockedBuffer
	done := make(chan error, 1)
	go func() {
		_, err := c.Exec(ctx, "web", []string{"sh"}, true, 24, 80, pr, &out, &out)
		done <- err
	}()

	const marker = "INTERACTIVE-MARKER"
	if _, err := pw.Write([]byte("echo " + marker + "\n")); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	// Output must stream back WHILE the session is still open (not buffered to exit).
	deadline := time.Now().Add(4 * time.Second)
	for !strings.Contains(out.String(), marker) {
		if time.Now().After(deadline) {
			t.Fatalf("interactive session did not stream output back; got %q", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Ask the shell to exit: the process ends, the server closes the session, and
	// the client's Exec returns cleanly.
	if _, err := pw.Write([]byte("exit\n")); err != nil {
		t.Fatalf("write exit: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("interactive exec returned error: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("interactive session did not end after the shell exited")
	}
	_ = pw.Close()
}

// lockedBuffer is a goroutine-safe bytes.Buffer for the streaming assertions.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestRestartKnownService: POST /v1/restart/<svc> bounces a known service and
// records the request; the client returns no error.
func TestRestartKnownService(t *testing.T) {
	sup := &fakeSupervisor{services: []spacemanager.ServiceSnapshot{{Name: "api"}}}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	if err := c.Restart(context.Background(), "api"); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if len(sup.restarted) != 1 || sup.restarted[0] != "api" {
		t.Fatalf("restarted = %v, want [api]", sup.restarted)
	}
}

// TestRestartUnknownService404: restarting a service the supervisor does not
// know yields a clear error (mapped from the server's 404).
func TestRestartUnknownService404(t *testing.T) {
	sup := &fakeSupervisor{services: []spacemanager.ServiceSnapshot{{Name: "api"}}}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	err := c.Restart(context.Background(), "ghost")
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("want 404 for unknown service, got %v", err)
	}
	if len(sup.restarted) != 0 {
		t.Fatalf("unknown service must not be restarted, got %v", sup.restarted)
	}
}

// TestRestartInvalidName: a name outside the DNS-label shape is rejected before
// any mutation (defence-in-depth, same gate as apply).
func TestRestartInvalidName(t *testing.T) {
	sup := &fakeSupervisor{services: []spacemanager.ServiceSnapshot{{Name: "api"}}}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	if err := c.Restart(context.Background(), "../../etc"); err == nil {
		t.Fatal("want error for invalid service name")
	}
	if len(sup.restarted) != 0 {
		t.Fatalf("invalid name must not reach RestartService, got %v", sup.restarted)
	}
}

// TestExecUnknownServiceErrorsBeforeHijack: exec on a service with no running
// instance (no execTarget) returns a 404 the client surfaces — and critically
// it happens BEFORE any connection takeover, so the failure is a clean error.
// Non-tty path so the standard client transport observes the 404 status.
func TestExecUnknownServiceErrorsBeforeHijack(t *testing.T) {
	sup := &fakeSupervisor{services: []spacemanager.ServiceSnapshot{{Name: "api"}}}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	_, err := c.Exec(context.Background(), "ghost", []string{"ls"}, false, 0, 0, nil, &strings.Builder{}, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("want error for service with no running instance, got %v", err)
	}
}

// TestExecInvalidName: an invalid service name is rejected up front.
func TestExecInvalidName(t *testing.T) {
	sup := &fakeSupervisor{services: []spacemanager.ServiceSnapshot{{Name: "api"}}}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	if _, err := c.Exec(context.Background(), "bad/name", nil, false, 0, 0, nil, &strings.Builder{}, &strings.Builder{}); err == nil {
		t.Fatal("want error for invalid service name")
	}
}

// TestRunJobKnown: POST /v1/run/<job> schedules a run of a terminal job.
func TestRunJobKnown(t *testing.T) {
	sup := &fakeSupervisor{services: []spacemanager.ServiceSnapshot{{Name: "migrate", Oneshot: true}}}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	if err := c.RunJob(context.Background(), "migrate"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(sup.jobRuns) != 1 || sup.jobRuns[0] != "migrate" {
		t.Fatalf("jobRuns = %v, want [migrate]", sup.jobRuns)
	}
}

// TestRunJobReasonsMapToStatus: the three TriggerJobErr reasons must reach the
// caller as distinct statuses — a run already in flight is a 409, not a 404
// ("no such job") and not a 502 ("the node said no").
func TestRunJobReasonsMapToStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		job  string
		errs map[string]error
		want int
	}{
		{"unknown", "ghost", nil, http.StatusNotFound},
		{"not a job", "api", map[string]error{"api": spacemanager.ErrJobNotOneshot}, http.StatusBadRequest},
		{"in flight", "migrate", map[string]error{"migrate": spacemanager.ErrJobInFlight}, http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sup := &fakeSupervisor{
				services: []spacemanager.ServiceSnapshot{{Name: "api"}, {Name: "migrate", Oneshot: true}},
				jobErrs:  tc.errs,
			}
			c, cancel := serveFake(t, "production", sup)
			defer cancel()

			err := c.RunJob(context.Background(), tc.job)
			if err == nil {
				t.Fatalf("want an error carrying status %d", tc.want)
			}
			if tc.want != http.StatusNotFound && !strings.Contains(err.Error(), strconv.Itoa(tc.want)) {
				t.Fatalf("error %q does not carry status %d", err, tc.want)
			}
			if len(sup.jobRuns) != 0 {
				t.Fatalf("no run must be scheduled, got %v", sup.jobRuns)
			}
		})
	}
}
