//go:build linux

package main

import (
	"bytes"
	"encoding/base64"
	"strings"
	"sync"
	"testing"
)

// The pipe path frames whole lines per stream so a caller redirecting stdout
// still gets stdout only.
func TestFramedWriterTagsEachLine(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	w := framedWriter{mu: &mu, w: &buf, tag: '2'}

	if _, err := w.Write([]byte("first\nsecond\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := buf.String()
	if got != "2first\n2second\n" {
		t.Errorf("framing = %q, want each line tagged", got)
	}
}

func TestFramedWriterSkipsEmptyTrailer(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	w := framedWriter{mu: &mu, w: &buf, tag: '1'}
	w.Write([]byte("only\n"))
	if n := strings.Count(buf.String(), "\n"); n != 1 {
		t.Errorf("emitted %d frames for one line, want 1", n)
	}
}

// Stdin is base64 so a keystroke that happens to be a newline cannot be read
// as a frame boundary.
func TestControlFramesSurviveNewlinePayload(t *testing.T) {
	payload := []byte("line one\nline two\n")
	frame := "I" + base64.StdEncoding.EncodeToString(payload)
	if strings.Count(frame, "\n") != 0 {
		t.Fatal("an encoded stdin frame must contain no newline")
	}
	back, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(frame, "I"))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !bytes.Equal(back, payload) {
		t.Errorf("round-trip = %q, want %q", back, payload)
	}
}

func TestGuestBaseEnvAlwaysCarriesPath(t *testing.T) {
	for _, tty := range []bool{false, true} {
		var hasPath, hasTTY bool
		for _, kv := range guestBaseEnv(tty) {
			if strings.HasPrefix(kv, "PATH=") {
				hasPath = true
			}
			if kv == "SKLP_TTY=1" {
				hasTTY = true
			}
		}
		if !hasPath {
			t.Errorf("tty=%v: PATH missing — PID 1 inherits no environment", tty)
		}
		if hasTTY != tty {
			t.Errorf("tty=%v: SKLP_TTY presence = %v, want %v", tty, hasTTY, tty)
		}
	}
}

// exec.Command resolves against the agent's own PATH, which is empty as PID 1;
// the lookup must use the guest PATH instead.
func TestGuestLookPathRejectsUnknown(t *testing.T) {
	if _, err := guestLookPath("definitely-not-installed-here"); err == nil {
		t.Fatal("an unknown binary must be reported, not passed to exec")
	}
}

func TestGuestLookPathPassesThroughAbsolute(t *testing.T) {
	got, err := guestLookPath("/usr/local/bin/sklp")
	if err != nil {
		t.Fatalf("absolute path: %v", err)
	}
	if got != "/usr/local/bin/sklp" {
		t.Errorf("got %q, want the path unchanged", got)
	}
}
