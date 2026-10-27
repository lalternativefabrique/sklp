package toolchain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Install downloads the artifact for (name, version) on the host's
// (os, arch), verifies its sha256, and extracts it into a stable
// per-tool subdirectory of cacheDir. It returns the absolute path of
// the resulting binary so the caller can prepend its parent to PATH.
//
// Caching: the final binary lives at <cacheDir>/<name>/<version>/...
// Subsequent calls with the same (name, version) skip the network
// entirely and return the cached path. Cache invalidation = delete the
// version subdirectory.
//
// Errors carry enough context to debug a failing install in CI:
// upstream URL, expected vs actual sha256, extracted destination.
func Install(ctx context.Context, name, version, cacheDir string) (string, error) {
	// name and version are pipeline-YAML values that become path segments just
	// below, and the install dir lands on the PATH of later steps. Validate
	// before anything else so a hostile pair is refused regardless of whether
	// the tool resolves in the registry.
	if err := ValidateToolRef(name, version); err != nil {
		return "", err
	}
	t, ok := Get(name)
	if !ok {
		return "", fmt.Errorf("toolchain: %q not in native registry", name)
	}
	if cacheDir == "" {
		return "", fmt.Errorf("toolchain: cacheDir required")
	}

	osName, arch := runtime.GOOS, runtime.GOARCH
	destDir := filepath.Join(cacheDir, name, version, osName+"-"+arch)

	// Cache hit: if the destination already has a marker file from a
	// previous successful install, trust it. The marker stores the
	// binary path so we don't have to re-derive it (extraction layouts
	// differ per tool: kubectl is a flat file, go is a directory).
	if bin, ok := readCacheMarker(destDir); ok {
		return bin, nil
	}

	url, err := t.URL(version, osName, arch)
	if err != nil {
		return "", fmt.Errorf("toolchain %s@%s: url: %w", name, version, err)
	}
	expectedSha, err := t.SHA256(version, osName, arch)
	if err != nil {
		return "", fmt.Errorf("toolchain %s@%s: sha256 lookup: %w", name, version, err)
	}

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", fmt.Errorf("toolchain %s@%s: mkdir %s: %w", name, version, destDir, err)
	}

	// Download into a sibling temp file so a partially-written archive
	// never poisons the cache. Rename-into-place only happens after
	// sha256 verification.
	tmpFile, err := os.CreateTemp(destDir, ".download-*")
	if err != nil {
		return "", fmt.Errorf("toolchain %s@%s: temp file: %w", name, version, err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath) // best-effort, in case extraction fails before cleanup

	if err := downloadWithChecksum(ctx, url, tmpFile, expectedSha); err != nil {
		tmpFile.Close()
		return "", fmt.Errorf("toolchain %s@%s: download %s: %w", name, version, url, err)
	}
	if err := tmpFile.Close(); err != nil {
		return "", fmt.Errorf("toolchain %s@%s: close tmp: %w", name, version, err)
	}

	bin, err := t.Extract(tmpPath, destDir)
	if err != nil {
		return "", fmt.Errorf("toolchain %s@%s: extract: %w", name, version, err)
	}
	if err := writeCacheMarker(destDir, bin); err != nil {
		// Marker failure is non-fatal — the binary still works for this
		// run. We log it as part of the error chain so an operator can
		// fix a permissions issue without losing the install.
		return bin, fmt.Errorf("toolchain %s@%s: marker write: %w (binary still usable at %s)", name, version, err, bin)
	}
	return bin, nil
}

// downloadWithChecksum streams url into w while computing sha256
// on the fly, then errors out if the digest doesn't match expectedHex.
// Streaming means a huge artifact never has to fit in memory.
func downloadWithChecksum(ctx context.Context, url string, w io.Writer, expectedHex string) error {
	expectedHex = strings.ToLower(strings.TrimSpace(expectedHex))
	if len(expectedHex) != 64 {
		return fmt.Errorf("expected sha256 must be 64 hex chars, got %q", expectedHex)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	h := sha256.New()
	mw := io.MultiWriter(w, h)
	if _, err := io.Copy(mw, resp.Body); err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	actual := hex.EncodeToString(h.Sum(nil))
	if actual != expectedHex {
		return fmt.Errorf("sha256 mismatch: want %s, got %s", expectedHex, actual)
	}
	return nil
}

// FetchFull is the unbounded sibling of FetchString. It returns the
// entire response body as a string, no truncation, no whitespace
// stripping. Exported so Tool implementations outside this package
// (tools/nodejs.go, tools/golang.go via fetchFullString legacy alias,
// future tools) can parse multi-line files like SHASUMS or release
// manifests.
func FetchFull(ctx context.Context, url string) (string, error) {
	return fetchFullString(ctx, url)
}

// fetchFullString is the package-private name kept as an alias for
// internal call sites. New callers should use FetchFull.
func fetchFullString(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d on %s", resp.StatusCode, url)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// FetchString does a GET on url and returns the trimmed body as a
// string. Used by Tool implementations that fetch the upstream's
// `*.sha256` artifact (the conventional way kubernetes, helm, go all
// publish checksums).
//
// Whitespace and the conventional "  filename" suffix that sha256sum
// produces are stripped — we want only the 64-char hex digest.
func FetchString(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d on %s", resp.StatusCode, url)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(body))
	// Some checksums files come as `<hex>  <filename>` (two spaces);
	// strip the filename to keep only the digest.
	if i := strings.IndexAny(s, " \t"); i > 0 {
		s = s[:i]
	}
	return s, nil
}

// cache marker — a tiny file written next to the extracted binary so
// subsequent Install calls can skip the network without re-parsing the
// extracted layout.
const markerName = ".sklp-bin"

func readCacheMarker(destDir string) (string, bool) {
	raw, err := os.ReadFile(filepath.Join(destDir, markerName))
	if err != nil {
		return "", false
	}
	bin := strings.TrimSpace(string(raw))
	if bin == "" {
		return "", false
	}
	// Sanity check: if the marker points at a missing file, treat as
	// cache miss so the next Install re-downloads.
	if _, err := os.Stat(bin); err != nil {
		return "", false
	}
	return bin, true
}

func writeCacheMarker(destDir, bin string) error {
	return os.WriteFile(filepath.Join(destDir, markerName), []byte(bin+"\n"), 0o644)
}
