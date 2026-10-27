package space

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRuncExec writes a shell script that stands in for the runc binary: it drops the
// leading `--root <root> exec [-t] <id>` arguments and runs the rest as the
// "in-container" command on the host, so RuncExecStreams/TTY can be exercised
// without a real container or privileges. Returns the script path.
func fakeRuncExec(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "runc")
	// Args layout: --root <root> exec [-t] <id> <cmd...>. Shift past them, then
	// exec the remaining command verbatim.
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

// TestRuncExecStreamsCapturesAndExitCode: the non-interactive exec runs the
// command, captures stdout, and propagates the exit code.
func TestRuncExecStreamsCapturesAndExitCode(t *testing.T) {
	runc := fakeRuncExec(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var out bytes.Buffer
	code, err := RuncExecStreams(ctx, runc, "/state", "cid", []string{"sh", "-c", "echo hello; exit 3"}, nil, &out, &out)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if code != 3 {
		t.Fatalf("exit code = %d, want 3", code)
	}
	if !strings.Contains(out.String(), "hello") {
		t.Fatalf("stdout = %q, want hello", out.String())
	}
}

// TestRuncExecStreamsStdin: stdin is wired through to the command.
func TestRuncExecStreamsStdin(t *testing.T) {
	runc := fakeRuncExec(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var out bytes.Buffer
	code, err := RuncExecStreams(ctx, runc, "/state", "cid", []string{"cat"}, strings.NewReader("piped-in"), &out, &out)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if out.String() != "piped-in" {
		t.Fatalf("stdout = %q, want piped-in", out.String())
	}
}

// rwPipe is a minimal in-memory ReadWriter: reads drain what was written by the
// peer side. Used to drive RuncExecTTY's bidi proxy in a test. out is guarded by
// mu because the PTY copy goroutine writes it while the test polls its contents.
type rwPipe struct {
	in  *bytes.Reader // bytes the "client" sends to the PTY
	mu  sync.Mutex
	out bytes.Buffer // bytes the PTY sends back (guarded by mu)
}

func (p *rwPipe) Read(b []byte) (int, error) { return p.in.Read(b) }

func (p *rwPipe) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.out.Write(b)
}

func (p *rwPipe) String() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.out.String()
}

// TestRuncExecTTYRoundTrip: the PTY-backed exec echoes its input back over the
// single bidi stream. Proves the hijack-equivalent proxy wiring works end to end
// (allocate PTY -> runc exec -t -> copy both directions) on this kernel.
func TestRuncExecTTYRoundTrip(t *testing.T) {
	runc := fakeRuncExec(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	peer := &rwPipe{in: bytes.NewReader([]byte("ping\n"))}
	// `cat` echoes stdin to stdout; under a PTY the line is mirrored back. cat
	// exits when the master closes (our slave EOF) — bounded by the ctx timeout.
	go func() {
		_, _ = RuncExecTTY(ctx, runc, "/state", "cid", []string{"cat"}, peer, 24, 80)
	}()

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(peer.String(), "ping") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("PTY round-trip never echoed input; got %q", peer.String())
}

// gatedPeer drives RuncExecTTY's bidi proxy and proves the output drain is
// AWAITED before the call returns. Reads return immediate EOF (a one-shot with
// no stdin, like `ls`). The first Write blocks until release() is called, then
// records — after the call returns — whether RuncExecTTY had returned while that
// first chunk of PTY output was still in flight. The real bug is exactly this:
// the server closes the hijacked conn the instant RuncExecTTY returns, so output
// not yet relayed is dropped. With the drain awaited, RuncExecTTY cannot return
// until this Write has completed.
type gatedPeer struct {
	gate     chan struct{} // closed by release() to let the first Write proceed
	once     sync.Once
	mu       sync.Mutex
	out      bytes.Buffer // bytes the PTY sent back (guarded by mu)
	returned bool         // set true when RuncExecTTY signals it returned
}

func newGatedPeer() *gatedPeer { return &gatedPeer{gate: make(chan struct{})} }

func (p *gatedPeer) release() { close(p.gate) }

func (p *gatedPeer) Read([]byte) (int, error) { return 0, io.EOF }

func (p *gatedPeer) Write(b []byte) (int, error) {
	// Block the FIRST chunk of output until the test releases the gate, so we
	// control the moment output is delivered relative to RuncExecTTY returning.
	p.once.Do(func() { <-p.gate })
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.returned {
		// RuncExecTTY returned before this output reached the peer: in production
		// the conn is already closed here and the bytes are lost. Fail loudly.
		return 0, errors.New("output written after RuncExecTTY returned (drain not awaited)")
	}
	return p.out.Write(b)
}

func (p *gatedPeer) markReturned() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.returned = true
}

func (p *gatedPeer) String() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.out.String()
}

// TestRuncExecTTYOneShotDrainsOutput: a one-shot command (writes then exits
// immediately, no stdin) must have its FULL output relayed to peer BEFORE
// RuncExecTTY returns. This is the interactive `sklp deploy exec web -- ls /`
// case: the server closes the hijacked conn the instant RuncExecTTY returns, so
// any PTY output not yet drained is lost. Deterministic: the peer gates its first
// write so the output is provably in flight when the (buggy) code would return.
func TestRuncExecTTYOneShotDrainsOutput(t *testing.T) {
	runc := fakeRuncExec(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const marker = "ONESHOT-OUTPUT-MARKER"
	peer := newGatedPeer()

	type result struct {
		code int
		err  error
	}
	resCh := make(chan result, 1)
	go func() {
		code, err := RuncExecTTY(ctx, runc, "/state", "cid", []string{"sh", "-c", "echo " + marker}, peer, 24, 80)
		// Mark "returned" BEFORE publishing the result: a write that lands after
		// this point (i.e. after the function returned) is a lost-output bug.
		peer.markReturned()
		resCh <- result{code, err}
	}()

	// Give the process time to exit and cmd.Wait to return while the first output
	// write is parked on the gate. Only then release it. If the drain is NOT
	// awaited, RuncExecTTY has already returned and markReturned ran, so the Write
	// errors; if it IS awaited, RuncExecTTY is still blocked on the drain.
	time.Sleep(200 * time.Millisecond)
	peer.release()

	select {
	case res := <-resCh:
		if res.err != nil {
			t.Fatalf("exec: %v", res.err)
		}
		if res.code != 0 {
			t.Fatalf("exit code = %d, want 0", res.code)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("RuncExecTTY did not return after the gate was released")
	}
	if !strings.Contains(peer.String(), marker) {
		t.Fatalf("one-shot output lost: peer = %q, want to contain %q", peer.String(), marker)
	}
}
