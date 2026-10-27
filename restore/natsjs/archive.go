package natsjs

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// extract unpacks a gzipped tar into dest, refusing any entry that would land
// outside it: the archive comes from a bucket, not from something trusted.
func extract(r io.Reader, dest string) (int64, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return 0, fmt.Errorf("not a gzip archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var written int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return written, nil
		}
		if err != nil {
			return written, fmt.Errorf("read archive: %w", err)
		}
		path, err := within(dest, hdr.Name)
		if err != nil {
			return written, err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o700); err != nil {
				return written, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return written, err
			}
			f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return written, err
			}
			n, err := io.Copy(f, tr)
			written += n
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return written, fmt.Errorf("write %s: %w", hdr.Name, err)
			}
		}
	}
}

func within(dest, name string) (string, error) {
	if filepath.IsAbs(name) || slices.Contains(strings.Split(filepath.ToSlash(name), "/"), "..") {
		return "", fmt.Errorf("archive entry %q escapes the restore directory", name)
	}
	return filepath.Join(dest, filepath.Clean(name)), nil
}

// storeDir finds the directory nats-server was pointed at with -sd: the one
// holding jetstream/. Archives are taken from whatever the app mounted, so it
// may sit one or two levels down.
func storeDir(root string) (string, error) {
	var found string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if strings.Count(rel, string(os.PathSeparator)) > 3 {
			return filepath.SkipDir
		}
		if d.Name() == "jetstream" {
			found = filepath.Dir(p)
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", errors.New("the archive holds no jetstream directory")
	}
	return found, nil
}

// onDisk lists account → streams as nats-server lays them out, so the drill
// can prove every stream in the archive came back, not just some.
func onDisk(store string) (map[string][]string, error) {
	accounts, err := os.ReadDir(filepath.Join(store, "jetstream"))
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, a := range accounts {
		if !a.IsDir() {
			continue
		}
		streams, err := os.ReadDir(filepath.Join(store, "jetstream", a.Name(), "streams"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, s := range streams {
			if s.IsDir() {
				out[a.Name()] = append(out[a.Name()], s.Name())
			}
		}
	}
	return out, nil
}
