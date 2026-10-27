package sklet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func versionScript(v string) []byte {
	return []byte("#!/bin/sh\necho " + v + "\n")
}

type fakeDist struct {
	binary []byte
	sums   string
}

func (d *fakeDist) serve(t *testing.T) *httptest.Server {
	t.Helper()
	name := "sklet-linux-" + runtime.GOARCH
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cli/v2/SHA256SUMS":
			io.WriteString(w, d.sums)
		case "/cli/v2/" + name:
			w.Write(d.binary)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newDist(binary []byte) *fakeDist {
	sum := sha256.Sum256(binary)
	return &fakeDist{binary: binary, sums: hex.EncodeToString(sum[:]) + "  sklet-linux-" + runtime.GOARCH + "\n"}
}

type fakeAgent struct {
	pid, restarts int
	crashLoop     bool
	restartCalls  int
}

func (a *fakeAgent) restart() error {
	a.restartCalls++
	a.pid += 100
	return nil
}

func (a *fakeAgent) state() (agentState, error) {
	if a.crashLoop {
		a.pid++
		a.restarts++
	}
	return agentState{Active: true, PID: a.pid, Restarts: a.restarts}, nil
}

func newUpgrader(t *testing.T, agent *fakeAgent, current string) *upgrader {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "sklet")
	if err := os.WriteFile(bin, versionScript(current), 0o755); err != nil {
		t.Fatal(err)
	}
	return &upgrader{
		bin:           bin,
		launcherReady: func() bool { return true },
		http:          http.DefaultClient,
		restart:       agent.restart,
		state:         agent.state,
		settle:        1500 * time.Millisecond,
		within:        6 * time.Second,
		out:           io.Discard,
	}
}

func versionOf(t *testing.T, path string) string {
	t.Helper()
	v, err := binaryVersion(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestUpgradeSwapsTheBinaryAndKeepsThePrevious(t *testing.T) {
	agent := &fakeAgent{pid: 10}
	u := newUpgrader(t, agent, "v1")
	srv := newDist(versionScript("v2")).serve(t)

	if err := u.run(context.Background(), UpgradeOptions{Version: "v2", Dist: srv.URL + "/cli"}); err != nil {
		t.Fatal(err)
	}
	if got := versionOf(t, u.bin); got != "v2" {
		t.Fatalf("agent binary = %s, want v2", got)
	}
	if got := versionOf(t, u.bin+".prev"); got != "v1" {
		t.Fatalf("previous binary = %s, want v1 kept for a manual rollback", got)
	}
	if agent.restartCalls != 1 {
		t.Fatalf("restarts = %d", agent.restartCalls)
	}
}

func TestUpgradeRefusesABinaryThatDoesNotMatchItsChecksum(t *testing.T) {
	agent := &fakeAgent{pid: 10}
	u := newUpgrader(t, agent, "v1")
	dist := newDist(versionScript("v2"))
	dist.binary = versionScript("tampered")
	srv := dist.serve(t)

	err := u.run(context.Background(), UpgradeOptions{Version: "v2", Dist: srv.URL + "/cli"})
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v", err)
	}
	if versionOf(t, u.bin) != "v1" || agent.restartCalls != 0 {
		t.Fatal("a rejected download touched the running agent")
	}
	if _, err := os.Stat(u.bin + ".new"); !os.IsNotExist(err) {
		t.Fatal("the rejected download was left on disk")
	}
}

func TestUpgradeIsANoOpOnTheSameVersion(t *testing.T) {
	agent := &fakeAgent{pid: 10}
	u := newUpgrader(t, agent, "v2")
	srv := newDist(versionScript("v2")).serve(t)
	if err := u.run(context.Background(), UpgradeOptions{Version: "v2", Dist: srv.URL + "/cli"}); err != nil {
		t.Fatal(err)
	}
	if agent.restartCalls != 0 {
		t.Fatal("an agent already on the version was restarted")
	}
}

func TestUpgradeRollsBackAnAgentThatDoesNotStayUp(t *testing.T) {
	agent := &fakeAgent{pid: 10, crashLoop: true}
	u := newUpgrader(t, agent, "v1")
	u.within = 3 * time.Second
	srv := newDist(versionScript("v2")).serve(t)

	err := u.run(context.Background(), UpgradeOptions{Version: "v2", Dist: srv.URL + "/cli"})
	if err == nil || !strings.Contains(err.Error(), "did not stay up") {
		t.Fatalf("err = %v", err)
	}
	if got := versionOf(t, u.bin); got != "v1" {
		t.Fatalf("after a failed upgrade the agent binary is %s, want v1 back", got)
	}
	if agent.restartCalls != 2 {
		t.Fatalf("restarts = %d, want the upgrade and the rollback", agent.restartCalls)
	}
}

func TestUpgradeNeedsTheLauncher(t *testing.T) {
	u := newUpgrader(t, &fakeAgent{}, "v1")
	u.launcherReady = func() bool { return false }
	err := u.run(context.Background(), UpgradeOptions{})
	if err == nil || !strings.Contains(err.Error(), "sudo sklet host setup") {
		t.Fatalf("err = %v", err)
	}
}

func TestChecksumForReadsSha256sumOutput(t *testing.T) {
	sums := "aaa  sklp-linux-amd64\nbbb *sklet-linux-amd64\n"
	if got, err := checksumFor(sums, "sklet-linux-amd64"); err != nil || got != "bbb" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := checksumFor(sums, "sklet-linux-arm64"); err == nil {
		t.Fatal("a missing entry was accepted")
	}
}

func TestLauncherRefusesATargetOthersCanWrite(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sklet")
	if err := os.WriteFile(p, versionScript("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ownedOnlyBy(p, os.Getuid()); err != nil {
		t.Fatalf("a private binary was refused: %v", err)
	}
	if err := os.Chmod(p, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := ownedOnlyBy(p, os.Getuid()); err == nil {
		t.Fatal("a world-writable binary would receive the capabilities")
	}
	if err := ownedOnlyBy(p, os.Getuid()+1); err == nil {
		t.Fatal("a binary owned by someone else would receive the capabilities")
	}
}

func TestRaiseAmbientReportsACapabilityTheLauncherLacks(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root holds every capability")
	}
	err := raiseAmbient([]uintptr{unix.CAP_NET_ADMIN})
	if err == nil || !strings.Contains(err.Error(), "sudo sklet host setup") {
		t.Fatalf("err = %v", err)
	}
}
