// secret_dotenv.go — dotenv parsing/writing for `sklp secret push/pull`.
//
// Format mirrors pipeline/pipeline_envfiles.go's parseDotenvFile
// exactly (KEY=value, `#` comments, blank lines, single/double-quote
// stripping, no ${VAR} expansion) so a file written by `secret pull` stays
// readable by a pipeline's env_files: block. Duplicated rather than
// imported: pipeline is a large, unrelated package, and pull needs
// an order/comment-preserving shape that pipeline's parser doesn't provide.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// parseDotenvValues reads a dotenv file into a flat KEY -> value map,
// discarding comments/blank lines/order. Missing file is not an error: an
// absent .env.secret before the first push is a valid empty state.
func parseDotenvValues(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	defer f.Close()

	out := map[string]string{}
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		k := strings.TrimSpace(line[:eq])
		v := stripQuotes(strings.TrimSpace(line[eq+1:]))
		out[k] = v
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// dotenvLine is one line of a dotenv file. Key == "" marks a pass-through
// line (comment, blank, unparsable) kept verbatim via Raw.
type dotenvLine struct {
	Raw   string
	Key   string
	Value string
}

// readDotenvLines reads a dotenv file preserving line order, comments, and
// blank lines, for pull's in-place merge. Missing file yields an empty slice.
func readDotenvLines(path string) ([]dotenvLine, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var lines []dotenvLine
	s := bufio.NewScanner(f)
	for s.Scan() {
		raw := s.Text()
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			lines = append(lines, dotenvLine{Raw: raw})
			continue
		}
		eq := strings.IndexByte(trimmed, '=')
		if eq <= 0 {
			lines = append(lines, dotenvLine{Raw: raw})
			continue
		}
		k := strings.TrimSpace(trimmed[:eq])
		v := stripQuotes(strings.TrimSpace(trimmed[eq+1:]))
		lines = append(lines, dotenvLine{Raw: raw, Key: k, Value: v})
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	return lines, nil
}

// mergeDotenvLines rewrites known keys in place with values from secrets and
// appends any key from secrets not already present. Lines whose key is not
// in secrets are left untouched — pull never deletes a local-only entry.
// Returns (updated, added) counts.
func mergeDotenvLines(lines *[]dotenvLine, secrets map[string]string) (updated, added int) {
	seen := map[string]bool{}
	for i, l := range *lines {
		if l.Key == "" {
			continue
		}
		if v, ok := secrets[l.Key]; ok {
			seen[l.Key] = true
			if l.Value != v {
				(*lines)[i] = dotenvLine{Raw: l.Key + "=" + quoteDotenvValue(v), Key: l.Key, Value: v}
				updated++
			}
		}
	}
	for k, v := range secrets {
		if seen[k] {
			continue
		}
		*lines = append(*lines, dotenvLine{Raw: k + "=" + quoteDotenvValue(v), Key: k, Value: v})
		added++
	}
	return updated, added
}

// writeDotenvLines writes lines back atomically (temp file + rename in the
// same directory) so a write failure never truncates the existing file.
func writeDotenvLines(path string, lines []dotenvLine) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	w := bufio.NewWriter(tmp)
	for _, l := range lines {
		if _, err := fmt.Fprintln(w, l.Raw); err != nil {
			tmp.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// quoteDotenvValue wraps v in double quotes when it contains whitespace or
// `#` (which would otherwise start a comment mid-line) — the inverse of
// stripQuotes.
func quoteDotenvValue(v string) string {
	if strings.ContainsAny(v, " \t#") {
		return `"` + strings.ReplaceAll(v, `"`, `\"`) + `"`
	}
	return v
}

// stripQuotes removes a matched pair of surrounding single or double
// quotes. Mirrors pipeline_envfiles.go's stripQuotes.
func stripQuotes(v string) string {
	if len(v) >= 2 {
		first, last := v[0], v[len(v)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}
