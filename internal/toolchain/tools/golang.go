package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/lalternativefabrique/sklp/internal/toolchain"
)

// fetchFullString GETs url and returns the entire response body as a
// string. Sibling to toolchain.FetchString but without the 1KB cap —
// the go manifest is ~50KB and needs the full body.
func fetchFullString(url string) (string, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
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

type golang struct{}

func init() { toolchain.Register(&golang{}) }

// Name returns "go". Matches the `tools.go` key in ci.yaml. Go's own
// canonical name for itself in pipelines is "go", so we stay aligned.
func (g *golang) Name() string { return "go" }

// URL points at go.dev's download CDN. Go publishes archives in a
// flat naming scheme keyed by version + os + arch.
//
// Layout: https://go.dev/dl/go<version>.<os>-<arch>.tar.gz
func (g *golang) URL(version, os, arch string) (string, error) {
	return fmt.Sprintf("https://go.dev/dl/go%s.%s-%s.tar.gz", version, os, arch), nil
}

// SHA256 walks go.dev's JSON release index to find the digest for
// (version, os, arch). Unlike kubectl/helm, go does NOT publish a
// per-archive .sha256 sibling — the only authoritative source is the
// release manifest at go.dev/dl/?mode=json&include=all.
//
// We fetch the manifest once per install. It's ~50KB and cacheable
// upstream, so the cost is negligible. The structure mirrors what
// `go install` and proto read.
func (g *golang) SHA256(version, osName, arch string) (string, error) {
	body, err := fetchFullString("https://go.dev/dl/?mode=json&include=all")
	if err != nil {
		return "", err
	}
	return goManifestSHA(version, osName, arch, body)
}

// goManifestSHA is split out so unit tests can feed a fixture manifest
// without hitting the network. The manifest shape is documented at
// https://go.dev/dl/?mode=json:
//
//	[
//	  {
//	    "version": "go1.25.0",
//	    "files": [
//	      { "filename": "go1.25.0.linux-amd64.tar.gz",
//	        "os": "linux", "arch": "amd64",
//	        "sha256": "deadbeef...", "kind": "archive" }
//	    ]
//	  }
//	]
//
// We match on (version, os, arch, kind=archive). Returning a clear
// "not found" error matters: a typo'd version otherwise looks like a
// network issue.
func goManifestSHA(version, osName, arch, manifestBody string) (string, error) {
	var releases []struct {
		Version string `json:"version"`
		Files   []struct {
			Filename string `json:"filename"`
			OS       string `json:"os"`
			Arch     string `json:"arch"`
			SHA256   string `json:"sha256"`
			Kind     string `json:"kind"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(manifestBody), &releases); err != nil {
		return "", fmt.Errorf("go manifest parse: %w", err)
	}

	want := "go" + version
	for _, r := range releases {
		if r.Version != want {
			continue
		}
		for _, f := range r.Files {
			if f.OS == osName && f.Arch == arch && f.Kind == "archive" {
				return f.SHA256, nil
			}
		}
	}
	return "", fmt.Errorf("go manifest: no archive for %s %s/%s", want, osName, arch)
}

// Extract unpacks the tar.gz; go's archive is a top-level "go/"
// directory containing "bin/go". We return the path of the binary so
// the caller prepends `destDir/go/bin` to PATH.
func (g *golang) Extract(archivePath, destDir string) (string, error) {
	files, err := toolchain.UntarGz(archivePath, destDir)
	if err != nil {
		return "", err
	}
	for _, f := range files {
		if strings.HasSuffix(f, filepath.Join("go", "bin", "go")) {
			return f, nil
		}
	}
	return "", fmt.Errorf("go binary not found in %s (extracted %d files)", archivePath, len(files))
}
