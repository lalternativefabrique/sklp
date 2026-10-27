package toolchain

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// sktInstallTimeout caps a single tool install. A hung download (slow or
// unreachable mirror, especially from inside a pipeline netns) must fail
// the step with a clear error instead of freezing the run indefinitely at
// "N step(s)" with no output.
const sktInstallTimeout = 3 * time.Minute

// bakedSktPath is where the runner image bakes the skt binary (see
// runnerimage.go). It is the fallback when skt is not on PATH — the CI
// Pod's curated sandbox env may not carry /usr/local/bin on PATH for the
// resolver's own exec, but the binary is always at this fixed location.
const bakedSktPath = "/usr/local/bin/skt"

// sktReport mirrors the JSON contract skt prints on `skt install … --json`
// (see apps/skt/src/engine/reporter.rs InstallReport). Only the fields
// sklp consumes are declared; extra fields are ignored by encoding/json.
type sktReport struct {
	Tool      string `json:"tool"`
	Requested string `json:"requested"`
	Resolved  string `json:"resolved"`
	BinDir    string `json:"bin_dir"`
	Bin       string `json:"bin"`
	Cached    bool   `json:"cached"`
}

// SktInstall resolves a tool that is not in the native Go registry by
// delegating to skt (apps/skt), the standalone toolchain manager baked
// into the runner image. It:
//
//  1. Locates a usable `skt` binary, preferring the host PATH (dev
//     machines) and falling back to the image-baked copy at
//     /usr/local/bin/skt.
//  2. Runs `skt install <name>@<version> --cache-dir <cacheDir> --json`,
//     which downloads + verifies + extracts the tool into the cache and
//     prints a one-line JSON report.
//  3. Parses the report and returns its bin_dir so the caller can prepend
//     it to PATH.
//
// skt is baked into the image rather than downloaded on demand, so a
// missing binary is a hard, actionable error rather than a trigger for
// a network download.
func SktInstall(ctx context.Context, name, version, cacheDir string, log ...io.Writer) (string, error) {
	return SktInstallTagged(ctx, name, version, "", cacheDir, log...)
}

// SktInstallTagged is SktInstall with Go build tags for the tools skt compiles
// rather than downloads. skt makes the tags part of its cache key, so a tagged
// build never comes back from a slot filled by an untagged one.
func SktInstallTagged(ctx context.Context, name, version, tags, cacheDir string, log ...io.Writer) (string, error) {
	// name and version come from pipeline YAML and are joined into the
	// "<name>@<version>" argv below.
	if err := ValidateSktRef(name, version); err != nil {
		return "", err
	}
	if cacheDir == "" {
		return "", fmt.Errorf("skt install: cacheDir required")
	}

	sktPath, err := EnsureSkt()
	if err != nil {
		return "", fmt.Errorf("skt install for %s@%s: %w", name, version, err)
	}

	// Cap the install so a hung download fails the step instead of freezing
	// the whole run with no output. A heartbeat every 10s tells the user the
	// resolver is downloading, not stuck (skt --json stays silent until done).
	ctx, cancel := context.WithTimeout(ctx, sktInstallTimeout)
	defer cancel()
	if len(log) > 0 && log[0] != nil {
		stop := startHeartbeat(ctx, log[0], fmt.Sprintf("[sklp] tools: still downloading %s@%s", name, version))
		defer stop()
	}

	spec := fmt.Sprintf("%s@%s", name, version)
	args := []string{"install", spec, "--cache-dir", cacheDir, "--json"}
	if tags != "" {
		args = append(args, "--tags", tags)
	}
	cmd := exec.CommandContext(ctx, sktPath, args...)
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("skt install %s: timed out after %s (download hung — check network reachability from the run env)", spec, sktInstallTimeout)
		}
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		return "", fmt.Errorf("skt install %s: %w\n%s", spec, err, stderr)
	}

	var report sktReport
	if jerr := json.Unmarshal(out, &report); jerr != nil {
		return "", fmt.Errorf("skt install %s: parse JSON report: %w\noutput: %s", spec, jerr, strings.TrimSpace(string(out)))
	}
	if report.BinDir == "" {
		return "", fmt.Errorf("skt install %s: report carried no bin_dir (output: %s)", spec, strings.TrimSpace(string(out)))
	}
	return report.BinDir, nil
}

// EnsureSkt returns the path to a usable skt binary. It prefers a `skt`
// on the host PATH (dev machines, where a developer may have a newer
// build) and falls back to the image-baked copy at /usr/local/bin/skt.
//
// Exposed (rather than private) so validators / debug commands can ask
// "where would sklp find skt?".
func EnsureSkt() (string, error) {
	if p, err := exec.LookPath("skt"); err == nil {
		return p, nil
	}
	if st, err := os.Stat(bakedSktPath); err == nil && !st.IsDir() {
		return bakedSktPath, nil
	}
	return "", fmt.Errorf("skt not found on PATH or at %s; it must be baked into the runner image (see `sklp runner build`)", bakedSktPath)
}

// startHeartbeat prints msg to w every 10s until the returned stop func is
// called or ctx is done. It gives a silent long-running install visible
// progress so the run never looks frozen. Returns a stop func the caller
// defers.
func startHeartbeat(ctx context.Context, w io.Writer, msg string) (stop func()) {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-t.C:
				fmt.Fprintln(w, msg+" …")
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}
