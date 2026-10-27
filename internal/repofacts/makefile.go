package repofacts

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// detectMakefile reads the root Makefile (or GNUmakefile) and
// enumerates its target names. We do NOT evaluate the Makefile or
// follow includes — the goal is to give the LLM a list of "things
// the user already wrote a command for", not to reimplement make.
//
// Returns nil when no Makefile is present at the root.
func detectMakefile(root string) *MakefileFacts {
	// Conventional Makefile names. Order matches make's lookup.
	for _, candidate := range []string{"GNUmakefile", "Makefile", "makefile"} {
		path := filepath.Join(root, candidate)
		if st, err := os.Stat(path); err == nil && !st.IsDir() {
			targets := parseMakefileTargets(path)
			return &MakefileFacts{
				Path:    candidate,
				Targets: targets,
			}
		}
	}
	return nil
}

// parseMakefileTargets scans the file for lines that look like
// "<target>: [<deps>...]". The rules:
//
//   - The line must NOT start with a TAB (TAB-prefixed lines are
//     command bodies, never target declarations).
//   - The target name must match [A-Za-z0-9_.-]+; anything fancier
//     is a pattern rule or a substitution and we skip it.
//   - Multiple targets per line ("a b c: deps") all get added.
//   - .PHONY directives ARE parsed because their targets are real;
//     a "test:" further down is the actual declaration.
//
// Duplicates are removed while preserving first-seen order.
func parseMakefileTargets(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	seen := map[string]bool{}
	var out []string

	sc := bufio.NewScanner(f)
	// Some Makefiles have very long .PHONY lines; bump the line limit
	// to 1 MB so we don't truncate them and miss targets.
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		// Command body — never a target line.
		if strings.HasPrefix(line, "\t") {
			continue
		}
		// Strip inline comments (everything after `#`). Doesn't handle
		// `#` inside quoted strings — Makefile comments rarely use
		// that and we'd rather under-detect than over-detect.
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// .PHONY: a b c → we want a, b, c. The colon is the marker.
		if strings.HasPrefix(line, ".PHONY:") {
			rest := strings.TrimPrefix(line, ".PHONY:")
			for _, t := range strings.Fields(rest) {
				// .PHONY targets can include line-continuations; strip
				// the trailing backslash that bufio gives us inside the
				// scan iteration.
				t = strings.TrimSuffix(t, "\\")
				if t != "" && !seen[t] && isMakeTargetName(t) {
					seen[t] = true
					out = append(out, t)
				}
			}
			continue
		}

		// Regular target line: "<targets>: <deps>".
		// Skip lines that look like variable assignments (foo := bar)
		// or include/export directives.
		if strings.HasPrefix(line, "include ") || strings.HasPrefix(line, "-include ") ||
			strings.HasPrefix(line, "export ") || strings.HasPrefix(line, "override ") {
			continue
		}
		if strings.Contains(line, "=") {
			eqIdx := strings.Index(line, "=")
			colonIdx := strings.Index(line, ":")
			// "FOO := bar" → eq before colon (or no colon). Skip.
			if colonIdx == -1 || eqIdx < colonIdx {
				continue
			}
		}

		colon := strings.Index(line, ":")
		if colon <= 0 {
			continue
		}
		targets := strings.Fields(line[:colon])
		for _, t := range targets {
			if !seen[t] && isMakeTargetName(t) {
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	return out
}

// isMakeTargetName filters out pattern rules (%.o), variable refs
// ($(...)), and other non-target strings the parser might catch.
func isMakeTargetName(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '_' || c == '-' || c == '.':
		default:
			return false
		}
	}
	// Names starting with `.` other than recognised special-targets
	// are mostly noise (.SUFFIXES, .DEFAULT) — keep them out.
	if strings.HasPrefix(s, ".") {
		return false
	}
	return true
}
