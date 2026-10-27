package tools

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lalternativefabrique/sklp/internal/toolchain"
)

type bun struct{}

func init() { toolchain.Register(&bun{}) }

// Name returns "bun". Matches the `tools.bun` key in pipeline YAML.
func (b *bun) Name() string { return "bun" }

// URL points at the GitHub release zip for bun. bun ships pre-built
// single-file binaries per (os, arch), packaged inside a per-platform
// zip. We pick the non-baseline, non-profile variant — i.e. the modern
// CPU build users get with `curl -fsSL bun.sh/install | bash`.
//
// Layout: https://github.com/oven-sh/bun/releases/download/bun-v<version>/bun-<os>-<arch>.zip
func (b *bun) URL(version, os, arch string) (string, error) {
	asset, err := bunAsset(os, arch)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("https://github.com/oven-sh/bun/releases/download/bun-v%s/%s", version, asset), nil
}

// SHA256 parses the SHASUMS256.txt manifest the bun release pipeline
// publishes once per tag. It's a multi-line file in `sha256sum -c`
// format: `<hex>  <filename>` per line. We pick the line matching the
// (os, arch) zip we'd download from URL().
//
// Returning the digest from the manifest (rather than embedding) keeps
// version bumps in `_base.yaml` working without a code change.
func (b *bun) SHA256(version, osName, arch string) (string, error) {
	asset, err := bunAsset(osName, arch)
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf("https://github.com/oven-sh/bun/releases/download/bun-v%s/SHASUMS256.txt", version)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	body, err := toolchain.FetchFull(ctx, url)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(body, "\n") {
		// Lines look like: `<64-hex>  bun-linux-x64.zip`. The separator
		// is two spaces per sha256sum convention; trim defensively in
		// case bun's tooling ever shifts to a single tab.
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if fields[len(fields)-1] == asset {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("checksum for %s not in SHASUMS256.txt of bun-v%s", asset, version)
}

// Extract unpacks the per-platform zip and copies its bun binary into
// destDir/bun with the executable bit set. Inside the zip, the binary
// lives at `bun-<os>-<arch>/bun` (one nested directory mirroring the
// asset name).
func (b *bun) Extract(archivePath, destDir string) (string, error) {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return "", fmt.Errorf("open zip: %w", err)
	}
	defer r.Close()

	bin := filepath.Join(destDir, "bun")
	for _, f := range r.File {
		// Match either "bun" or any single nested "<prefix>/bun" entry —
		// we don't hard-code the prefix so future renames of the asset
		// directory (e.g. baseline suffix) keep working.
		base := filepath.Base(f.Name)
		if base != "bun" {
			continue
		}
		if f.FileInfo().IsDir() {
			continue
		}
		src, err := f.Open()
		if err != nil {
			return "", fmt.Errorf("open zip entry %s: %w", f.Name, err)
		}
		out, err := os.OpenFile(bin, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			src.Close()
			return "", fmt.Errorf("create binary: %w", err)
		}
		if _, err := io.Copy(out, src); err != nil {
			src.Close()
			out.Close()
			return "", fmt.Errorf("write binary: %w", err)
		}
		src.Close()
		out.Close()
		return bin, nil
	}
	return "", fmt.Errorf("bun binary not found in zip %s", archivePath)
}

// bunAsset returns the release zip filename for (os, arch). bun
// publishes several CPU variants per platform; we use the modern
// (non-baseline) build because the K8s nodes and dev workstations we
// target all have AVX2.
func bunAsset(goos, goarch string) (string, error) {
	osSlug, ok := bunOS(goos)
	if !ok {
		return "", fmt.Errorf("bun: unsupported os %q", goos)
	}
	archSlug, ok := bunArch(goarch)
	if !ok {
		return "", fmt.Errorf("bun: unsupported arch %q", goarch)
	}
	return fmt.Sprintf("bun-%s-%s.zip", osSlug, archSlug), nil
}

func bunOS(goos string) (string, bool) {
	switch goos {
	case "linux", "darwin", "windows":
		return goos, true
	default:
		return "", false
	}
}

// bunArch maps GOARCH to bun's release naming. bun uses "aarch64"
// instead of arm64 — the only diff from node/pnpm conventions.
func bunArch(goarch string) (string, bool) {
	switch goarch {
	case "amd64":
		return "x64", true
	case "arm64":
		return "aarch64", true
	default:
		return "", false
	}
}
