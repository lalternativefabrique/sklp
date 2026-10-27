// Package tools holds the native Tool implementations registered
// with toolchain at init time. One file per tool. The package import
// alone is the public API — callers don't reference these types
// directly, they go through toolchain.Get().
package tools

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/lalternativefabrique/sklp/internal/toolchain"
)

type kubectl struct{}

func init() { toolchain.Register(&kubectl{}) }

// Name returns "kubectl". Matches the `tools.kubectl` key in ci.yaml.
func (k *kubectl) Name() string { return "kubectl" }

// URL points at the official kubernetes release CDN. The version is
// passed without a leading "v" in ci.yaml ("1.32.0") and we prepend
// it here so users don't have to remember the convention.
//
// Layout: https://dl.k8s.io/release/v<version>/bin/<os>/<arch>/kubectl
func (k *kubectl) URL(version, os, arch string) (string, error) {
	return fmt.Sprintf("https://dl.k8s.io/release/v%s/bin/%s/%s/kubectl", version, os, arch), nil
}

// SHA256 fetches the sibling .sha256 file the kubernetes release
// process publishes for every binary. Same URL pattern with a
// .sha256 suffix.
//
// We fetch at install time rather than embedding the digest in code
// because (a) the file is small, (b) embedding means a code change
// per kubectl release, which kills the "just declare a version in
// ci.yaml" UX we're going for.
func (k *kubectl) SHA256(version, osName, arch string) (string, error) {
	url := fmt.Sprintf("https://dl.k8s.io/release/v%s/bin/%s/%s/kubectl.sha256", version, osName, arch)
	return toolchain.FetchString(context.Background(), url)
}

// Extract takes the verified raw binary and copies it into
// destDir/kubectl with the executable bit set. kubectl ships as a
// single file, not an archive — the simplest case.
func (k *kubectl) Extract(archivePath, destDir string) (string, error) {
	bin := filepath.Join(destDir, "kubectl")
	if err := toolchain.CopyBinary(archivePath, bin); err != nil {
		return "", err
	}
	return bin, nil
}
