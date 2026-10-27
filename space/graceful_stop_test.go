package space

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeRuncGraceful writes a runc stand-in whose `kill <id> SIGTERM` marks the
// container "stopped" after delay (simulating an app that finishes its
// in-flight work and exits on its own), and whose `kill <id> SIGKILL` marks it
// "stopped" immediately. `state <id>` reports the current status from a
// per-id marker file: absent = still running, present = stopped. This lets a
// test assert killContainer's timing without a real process.
func fakeRuncGraceful(t *testing.T, delay time.Duration) (binPath string, stateDir string) {
	t.Helper()
	dir := t.TempDir()
	stateDir = t.TempDir()
	binPath = filepath.Join(dir, "fake-runc")
	sleepSecs := delay.Seconds()
	script := fmt.Sprintf(`#!/bin/sh
op=""
sig=""
id=""
prev=""
for a in "$@"; do
  if [ -z "$op" ] && [ "$prev" != "--root" ] && [ "${a#--}" = "$a" ]; then op="$a"; fi
  prev="$a"
done
for a in "$@"; do
  case "$a" in
    SIGTERM|SIGKILL) sig="$a" ;;
    --*) ;;
    *) id="$a" ;;
  esac
done
marker="%s/$id.stopped"
case "$op" in
  kill)
    if [ "$sig" = "SIGKILL" ]; then
      : > "$marker"
    elif [ "$sig" = "SIGTERM" ]; then
      ( sleep %f; : > "$marker" ) &
    fi
    ;;
  state)
    if [ -f "$marker" ]; then
      echo '{"status":"stopped"}'
    else
      echo '{"status":"running"}'
    fi
    ;;
  delete)
    rm -f "$marker"
    ;;
esac
exit 0
`, stateDir, sleepSecs)
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake runc: %v", err)
	}
	return binPath, stateDir
}

// TestKillContainer_FastExitDoesNotWaitFullGracePeriod proves the grace
// period is a CEILING, not a fixed pause: a container that stops shortly
// after SIGTERM is deleted almost immediately, well under stopGracePeriod.
func TestKillContainer_FastExitDoesNotWaitFullGracePeriod(t *testing.T) {
	bin, stateDir := fakeRuncGraceful(t, 50*time.Millisecond)
	id := "svc-fast"

	start := time.Now()
	killContainer(bin, stateDir, id)
	elapsed := time.Since(start)

	if elapsed >= stopGracePeriod {
		t.Fatalf("killContainer took %v, at or above the full grace period %v — a fast exit must not wait it out", elapsed, stopGracePeriod)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("killContainer took %v for a container that stopped after 50ms — polling latency too high", elapsed)
	}
}

// TestKillContainer_SlowButGracefulExitIsNotForceKilled proves a container
// that takes several poll intervals to exit on its own (simulating an
// in-flight request finishing) is NOT SIGKILLed — it gets the time it needs,
// up to the cap.
func TestKillContainer_SlowButGracefulExitIsNotForceKilled(t *testing.T) {
	delay := 600 * time.Millisecond
	bin, stateDir := fakeRuncGraceful(t, delay)
	id := "svc-slow-graceful"

	start := time.Now()
	killContainer(bin, stateDir, id)
	elapsed := time.Since(start)

	if elapsed < delay {
		t.Fatalf("killContainer returned after %v, before the container's own %v exit — it must not race ahead", elapsed, delay)
	}
	if elapsed >= stopGracePeriod {
		t.Fatalf("killContainer took %v — a container that exits well within the grace period must not hit the SIGKILL path", elapsed)
	}
}
