package repofacts

import (
	"path/filepath"
	"strings"
)

// hasFile reports whether root contains a file with the given
// basename at depth 1 (i.e. root/<basename>). Used by detectors to
// check for canonical pin / lock files without re-walking.
func hasFile(files []string, root, basename string) bool {
	want := filepath.Join(root, basename)
	for _, p := range files {
		if p == want {
			return true
		}
	}
	return false
}

// findFileAt returns the absolute path of root/<basename> if it
// exists in the file list, or "" otherwise.
func findFileAt(files []string, root, basename string) string {
	want := filepath.Join(root, basename)
	for _, p := range files {
		if p == want {
			return p
		}
	}
	return ""
}

// pathContainsSegment reports whether path contains the given
// directory name as one of its segments. Used by detectors that need
// to filter out files inside node_modules / vendor even if the
// walker missed an exclusion (defensive).
func pathContainsSegment(path, segment string) bool {
	parts := strings.Split(filepath.ToSlash(path), "/")
	for _, p := range parts {
		if p == segment {
			return true
		}
	}
	return false
}

// trimSpace strips leading/trailing whitespace from a string. Wraps
// strings.TrimSpace so callers don't all need to import strings
// individually for a one-liner.
func trimSpace(s string) string { return strings.TrimSpace(s) }
