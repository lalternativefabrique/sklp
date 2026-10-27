// Command gen downloads the pinned OCI helper binaries (runc, slirp4netns)
// into the embed package dir and verifies each against its pinned sha256.
// Invoked by `go generate ./apps/sklp/space/embed/`. Binaries are
// gitignored — they exist only to satisfy //go:embed at build time.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/lalternativefabrique/sklp/space/embed/meta"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "embed/gen: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Write next to this package (the //go:embed targets live here), which is
	// the parent of the gen dir holding this file.
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		return fmt.Errorf("cannot resolve caller path")
	}
	dst := filepath.Dir(filepath.Dir(self)) // .../embed/gen -> .../embed

	for _, a := range meta.Assets {
		out := filepath.Join(dst, a.Name)
		if ok, _ := fileMatches(out, a.SHA256); ok {
			fmt.Printf("embed/gen: %s up to date\n", a.Name)
			continue
		}
		fmt.Printf("embed/gen: fetching %s from %s\n", a.Name, a.URL)
		if err := download(a.URL, out, a.SHA256); err != nil {
			return fmt.Errorf("%s: %w", a.Name, err)
		}
		fmt.Printf("embed/gen: wrote %s (sha256 ok)\n", a.Name)
	}
	return nil
}

// download fetches url to path, verifies sha256, and chmods 0755. The temp
// file is removed on any failure so a bad download never satisfies //go:embed.
func download(url, path, want string) error {
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %s", url, resp.Status)
	}

	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := verifySHA256(hex.EncodeToString(h.Sum(nil)), want); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// verifySHA256 reports a clear error when a downloaded asset's hash does not
// match the pinned one.
func verifySHA256(got, want string) error {
	if got != want {
		return fmt.Errorf("sha256 mismatch: got %s want %s", got, want)
	}
	return nil
}

// fileMatches reports whether path exists and hashes to want.
func fileMatches(path, want string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == want, nil
}
