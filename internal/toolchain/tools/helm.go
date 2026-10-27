package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/lalternativefabrique/sklp/internal/toolchain"
)

type helm struct{}

func init() { toolchain.Register(&helm{}) }

// Name returns "helm". Matches the `tools.helm` key in ci.yaml.
func (h *helm) Name() string { return "helm" }

// URL points at get.helm.sh, the official distribution endpoint. helm
// ships as a tar.gz that contains a top-level <os>-<arch>/ directory
// with the helm binary inside.
//
// Layout: https://get.helm.sh/helm-v<version>-<os>-<arch>.tar.gz
func (h *helm) URL(version, os, arch string) (string, error) {
	return fmt.Sprintf("https://get.helm.sh/helm-v%s-%s-%s.tar.gz", version, os, arch), nil
}

// SHA256 fetches the sibling .sha256 file helm publishes for every
// archive. Same URL pattern with .sha256 appended.
func (h *helm) SHA256(version, osName, arch string) (string, error) {
	url := fmt.Sprintf("https://get.helm.sh/helm-v%s-%s-%s.tar.gz.sha256", version, osName, arch)
	return toolchain.FetchString(context.Background(), url)
}

// Extract unpacks the tar.gz and returns the path of the helm binary
// inside the extracted <os>-<arch>/ subdirectory. We scan the file
// list rather than hard-coding the layout so that an upstream change
// (rare but possible) surfaces as a clear error instead of a silent
// "binary not found" later.
func (h *helm) Extract(archivePath, destDir string) (string, error) {
	files, err := toolchain.UntarGz(archivePath, destDir)
	if err != nil {
		return "", err
	}
	for _, f := range files {
		// helm-v3.16.0-linux-amd64.tar.gz extracts to:
		//   linux-amd64/helm
		//   linux-amd64/LICENSE
		//   linux-amd64/README.md
		// So we look for any file whose basename is exactly "helm".
		if strings.HasSuffix(f, string(filepath.Separator)+"helm") {
			return f, nil
		}
	}
	return "", fmt.Errorf("helm binary not found in %s (extracted %d files)", archivePath, len(files))
}
