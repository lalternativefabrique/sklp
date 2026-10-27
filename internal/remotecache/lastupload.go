package remotecache

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LastUploadPath returns the marker file used to record the timestamp
// of the most recent successful PutResult. Lives next to the local
// cache so `sklp cache status` can surface it without any network call.
func LastUploadPath(repoAbs string) string {
	return filepath.Join(repoAbs, ".sklp", "cache", ".last-upload")
}

// TouchLastUpload writes the current UTC time (RFC3339) to the marker
// file. Errors are non-fatal at the call site — the upload itself
// already succeeded by the time we get here.
func TouchLastUpload(repoAbs string) error {
	p := LastUploadPath(repoAbs)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(time.Now().UTC().Format(time.RFC3339)), 0o600)
}

// ReadLastUpload returns the recorded timestamp, or zero time when the
// file is missing / unreadable.
func ReadLastUpload(repoAbs string) (time.Time, error) {
	b, err := os.ReadFile(LastUploadPath(repoAbs))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return time.Time{}, nil
		}
		return time.Time{}, err
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(b)))
	if err != nil {
		return time.Time{}, err
	}
	return t, nil
}
