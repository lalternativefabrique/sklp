package toolchain

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ulikunitz/xz"
)

// CopyBinary makes a flat single-binary artifact (kubectl, pnpm) live
// at destPath with the executable bit set. Suitable for tools whose
// upstream publishes the raw binary, no archive.
func CopyBinary(srcPath, destPath string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return err
	}
	dst, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	return dst.Close()
}

// UntarGz extracts a gzip-compressed tar archive into destDir. Thin
// wrapper around untarReader that picks the right decompressor.
func UntarGz(archivePath, destDir string) ([]string, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()

	return untarReader(gz, destDir)
}

// UntarXz extracts an xz-compressed tar archive into destDir. Used by
// the node tool whose upstream publishes .tar.xz instead of .tar.gz on
// linux/darwin.
func UntarXz(archivePath, destDir string) ([]string, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	xr, err := xz.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("xz: %w", err)
	}
	return untarReader(xr, destDir)
}

// untarReader is the shared tar walker used by UntarGz and UntarXz.
// Returns the absolute paths of every regular file extracted, in
// archive order — Tool.Extract implementations use the list to find
// the binary they care about (`bin/helm`, `go/bin/go`, …) without
// having to re-walk the directory.
//
// Security: paths are sanitised against `..` traversal — anything that
// would escape destDir is rejected with a clear error. We don't support
// symlinks (none of our tracked tools ship any), so they're skipped.
func untarReader(r io.Reader, destDir string) ([]string, error) {
	tr := tar.NewReader(r)
	var extracted []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return extracted, fmt.Errorf("tar next: %w", err)
		}

		// Refuse path traversal. We use filepath.Clean + a prefix check
		// rather than just splitting on `..` so that legitimate names
		// containing two dots ("..apk-style.txt") still pass.
		clean := filepath.Clean(hdr.Name)
		if strings.HasPrefix(clean, "..") || strings.Contains(clean, string(os.PathSeparator)+"..") {
			return extracted, fmt.Errorf("tar entry escapes destDir: %s", hdr.Name)
		}
		target := filepath.Join(destDir, clean)

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return extracted, fmt.Errorf("mkdir %s: %w", target, err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return extracted, fmt.Errorf("mkdir %s: %w", filepath.Dir(target), err)
			}
			mode := os.FileMode(hdr.Mode) & 0o777
			if mode == 0 {
				mode = 0o644
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return extracted, fmt.Errorf("create %s: %w", target, err)
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return extracted, fmt.Errorf("copy %s: %w", target, err)
			}
			if err := out.Close(); err != nil {
				return extracted, err
			}
			extracted = append(extracted, target)
		default:
			// Symlinks, char/block devices, FIFOs — none of our tools
			// ship any. Silently skip rather than fail so future tools
			// can lean on this helper without immediate breakage.
			continue
		}
	}
	return extracted, nil
}
