// Package runner — pipeline-level env_files: block loading.
//
// 427: env_files for kind: pipeline
//
// LoadPipelineEnvFiles parses each dotenv file in order and exports
// non-conflicting keys into the runner's process environment via
// os.Setenv. Host env wins over file content: a CI runner that
// explicitly exports `GITHUB_TOKEN` in its environment must not be
// silently overridden by a checked-in `.env`. Within the file list
// itself, last-wins-on-collision matches standard dotenv conventions.
//
// Missing files are silently skipped — a `.env.local` that does not
// exist is fine by design.
//
// The function mutates process env (rather than threading a map through
// the executor) because the values must be visible to every downstream
// reader: EvaluateRequire's `git` subprocess, resolvePipelineTag's git
// calls, BuildKit secret resolution via os.LookupEnv, and the BaseEnv
// snapshot fed to the sandbox.
package pipeline

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// LoadPipelineEnvFiles reads each path in order, parses dotenv content,
// and applies host-env-wins precedence: a key already present in the
// host environment is preserved; a key absent from the host is set from
// the file content (last file wins on key collision among the files
// themselves). Relative paths are resolved against repoRoot.
//
// One info line is written to stderr per file actually loaded — only
// the file path and variable count, never values. Returns the resolved
// map of every key referenced by at least one file, with host-wins
// applied (host value when set, file value otherwise). Callers fold
// this map into the pipeline-level Env so values reach the sandbox.
func LoadPipelineEnvFiles(paths []string, repoRoot string, stderr io.Writer) (map[string]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	// merged tracks the file-side resolution (last-wins among files)
	// before host-env-wins is applied.
	merged := map[string]string{}
	loadedPerFile := make([]int, 0, len(paths))
	loadedPaths := make([]string, 0, len(paths))
	for _, p := range paths {
		abs := p
		if !filepath.IsAbs(abs) && repoRoot != "" {
			abs = filepath.Join(repoRoot, abs)
		}
		kv, err := parseDotenvFile(abs)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("env_files: load %s: %w", p, err)
		}
		for k, v := range kv {
			merged[k] = v
		}
		loadedPerFile = append(loadedPerFile, len(kv))
		loadedPaths = append(loadedPaths, p)
	}

	resolved := make(map[string]string, len(merged))
	for k, v := range merged {
		if hostVal, exists := os.LookupEnv(k); exists {
			// Host wins: keep the existing process env value, and
			// surface it to the caller so the sandbox sees the same
			// value the user explicitly exported.
			resolved[k] = hostVal
			continue
		}
		if err := os.Setenv(k, v); err != nil {
			return nil, fmt.Errorf("env_files: setenv %s: %w", k, err)
		}
		resolved[k] = v
	}

	if stderr != nil {
		for i, p := range loadedPaths {
			fmt.Fprintf(stderr, "[sklp] loaded env from %s (%d variables)\n", p, loadedPerFile[i])
		}
	}
	return resolved, nil
}

// parseDotenvFile reads one dotenv file. Format: KEY=value per line,
// blank lines ignored, `#` starts a comment, single/double quotes
// around the value are stripped. No `${VAR}` expansion. Mirrors the
// devstack parser so the two stay behaviourally identical.
func parseDotenvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
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
		v := strings.TrimSpace(line[eq+1:])
		v = stripQuotes(v)
		out[k] = v
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// stripQuotes removes a matched pair of surrounding single or double
// quotes. Unmatched quotes are preserved verbatim — the parser is
// intentionally boring.
func stripQuotes(v string) string {
	if len(v) >= 2 {
		first, last := v[0], v[len(v)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}
