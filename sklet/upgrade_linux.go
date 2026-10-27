package sklet

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const DefaultDistURL = "https://skalpai-dist.s3.fr-par.scw.cloud/cli"

type UpgradeOptions struct {
	Version string
	Dist    string
	Force   bool
}

type agentState struct {
	Active   bool
	PID      int
	Restarts int
}

type upgrader struct {
	bin            string
	launcherReady  func() bool
	http           *http.Client
	restart        func() error
	state          func() (agentState, error)
	settle, within time.Duration
	out            io.Writer
}

func RunUpgrade(ctx context.Context, opts UpgradeOptions) int {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklet upgrade:", err)
		return 1
	}
	u := &upgrader{
		bin:           AgentBinaryPath(home),
		launcherReady: func() bool { return LauncherReady(home) },
		http:          &http.Client{Timeout: 5 * time.Minute},
		restart: func() error {
			unit, err := renderAgentUnit()
			if err != nil {
				return err
			}
			return installAgentUnit(unit)
		},
		state:  systemdAgentState,
		settle: 20 * time.Second,
		within: 2 * time.Minute,
		out:    os.Stdout,
	}
	if err := u.run(ctx, opts); err != nil {
		fmt.Fprintln(os.Stderr, "sklet upgrade:", err)
		return 1
	}
	return 0
}

func (u *upgrader) run(ctx context.Context, opts UpgradeOptions) error {
	if !u.launcherReady() {
		return errors.New("no launcher on this node: run `sudo sklet host setup` once, then `sklet host configure`; upgrades need no sudo after that")
	}
	if opts.Version == "" {
		opts.Version = "latest"
	}
	if opts.Dist == "" {
		opts.Dist = DefaultDistURL
	}
	base := strings.TrimSuffix(opts.Dist, "/") + "/" + opts.Version
	name := "sklet-linux-" + runtime.GOARCH

	sums, err := u.fetch(ctx, base+"/SHA256SUMS")
	if err != nil {
		return fmt.Errorf("checksums: %w", err)
	}
	want, err := checksumFor(string(sums), name)
	if err != nil {
		return err
	}
	next := u.bin + ".new"
	if err := u.download(ctx, base+"/"+name, next, want); err != nil {
		os.Remove(next)
		return err
	}
	newVersion, err := binaryVersion(ctx, next)
	if err != nil {
		os.Remove(next)
		return fmt.Errorf("the downloaded sklet does not run: %w", err)
	}
	current, _ := binaryVersion(ctx, u.bin)
	if current == newVersion && !opts.Force {
		os.Remove(next)
		fmt.Fprintf(u.out, "sklet upgrade: already on %s\n", current)
		return nil
	}

	before, err := u.state()
	if err != nil {
		os.Remove(next)
		return fmt.Errorf("read agent state: %w", err)
	}
	prev := u.bin + ".prev"
	if err := os.Rename(u.bin, prev); err != nil {
		os.Remove(next)
		return err
	}
	if err := os.Rename(next, u.bin); err != nil {
		return errors.Join(err, os.Rename(prev, u.bin))
	}
	fmt.Fprintf(u.out, "sklet upgrade: %s → %s, restarting the agent\n", orUnknown(current), newVersion)
	if err := u.restartHealthy(ctx, before.PID); err != nil {
		return u.rollback(ctx, prev, before.PID, err)
	}
	fmt.Fprintf(u.out, "sklet upgrade: agent healthy on %s (previous binary kept at %s)\n", newVersion, prev)
	return nil
}

func (u *upgrader) rollback(ctx context.Context, prev string, oldPID int, cause error) error {
	fmt.Fprintf(u.out, "sklet upgrade: %v — rolling back\n", cause)
	if err := os.Rename(prev, u.bin); err != nil {
		return fmt.Errorf("%w; rollback failed too: %v", cause, err)
	}
	if err := u.restartHealthy(ctx, oldPID); err != nil {
		return fmt.Errorf("%w; the previous binary is back but the agent is not healthy: %v", cause, err)
	}
	return fmt.Errorf("%w; rolled back to the previous binary, agent healthy", cause)
}

// restartHealthy restarts the agent and waits until a new process has stayed
// up for the settle window: an agent that crash-loops is up for an instant
// between each restart, which a single check would take for success.
func (u *upgrader) restartHealthy(ctx context.Context, oldPID int) error {
	if err := u.restart(); err != nil {
		return err
	}
	deadline := time.Now().Add(u.within)
	var steadySince time.Time
	var steady agentState
	for time.Now().Before(deadline) {
		st, err := u.state()
		switch {
		case err != nil || !st.Active || st.PID == 0 || st.PID == oldPID:
			steadySince = time.Time{}
		case steadySince.IsZero() || st.PID != steady.PID || st.Restarts != steady.Restarts:
			steadySince, steady = time.Now(), st
		case time.Since(steadySince) >= u.settle:
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("the agent did not stay up for %s within %s", u.settle, u.within)
}

func (u *upgrader) fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := u.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

func (u *upgrader) download(ctx context.Context, url, dest, wantSHA string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := u.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantSHA {
		return fmt.Errorf("checksum mismatch for %s: got %s, SHA256SUMS says %s", url, got, wantSHA)
	}
	return nil
}

func checksumFor(sums, name string) (string, error) {
	sc := bufio.NewScanner(strings.NewReader(sums))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("SHA256SUMS lists no %s", name)
}

func binaryVersion(ctx context.Context, path string) (string, error) {
	out, err := exec.CommandContext(ctx, path, "version", "--short").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func systemdAgentState() (agentState, error) {
	out, err := exec.Command("systemctl", "--user", "show", agentUnitName, "-p", "ActiveState", "-p", "MainPID", "-p", "NRestarts").Output()
	if err != nil {
		return agentState{}, err
	}
	var st agentState
	for _, line := range strings.Split(string(out), "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch k {
		case "ActiveState":
			st.Active = v == "active"
		case "MainPID":
			st.PID, _ = strconv.Atoi(v)
		case "NRestarts":
			st.Restarts, _ = strconv.Atoi(v)
		}
	}
	return st, nil
}

func orUnknown(v string) string {
	if v == "" {
		return "unknown version"
	}
	return v
}
