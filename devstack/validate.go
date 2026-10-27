package devstack

import (
	"bytes"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"

	"github.com/lalternativefabrique/sklp/pipeline"
	"gopkg.in/yaml.v3"
)

// strictParse re-parses the file with KnownFields(true) against a
// devstack-only schema. Any unknown field triggers a user-friendly
// error (path inside the doc, line:col, "did you mean", list of valid
// fields) and — when the suggestion is high-confidence — applies an
// in-place fix and reports the diff. Returns:
//
//   - applied: true when the file was rewritten with corrections
//   - err: non-nil when the file is still invalid after auto-fixes
//
// We never expose Go type names (pipeline.Service, etc.) in error
// messages — only YAML paths and field names.
func strictParse(path string, autoFix bool) (applied bool, err error) {
	raw, readErr := os.ReadFile(path)
	if readErr != nil {
		return false, readErr
	}

	// Two-pass: try strict, collect issues, optionally rewrite, then
	// retry. Bounded to one rewrite cycle so a malformed file can't
	// loop forever.
	for pass := 0; pass < 2; pass++ {
		var doc devstackDoc
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		dec.KnownFields(true)
		if err := dec.Decode(&doc); err == nil {
			return applied, nil
		} else {
			issues := parseStrictErrors(err.Error())
			if len(issues) == 0 {
				// Not an unknown-field error — surface as-is, nothing to fix.
				return applied, friendlyParseError(path, err)
			}
			if !autoFix || pass == 1 {
				return applied, formatIssues(path, raw, issues)
			}
			fixed, anyFixed := applyAutoFixes(raw, issues)
			if !anyFixed {
				return applied, formatIssues(path, raw, issues)
			}
			if err := os.WriteFile(path, fixed, 0o644); err != nil {
				return applied, fmt.Errorf("write fixed file: %w", err)
			}
			diff := summariseFixes(issues)
			fmt.Fprintf(os.Stderr, "✓ auto-fixed %s:\n%s", path, diff)
			raw = fixed
			applied = true
		}
	}
	return applied, nil
}

// parseAllowHost reads the root `allowHost:` flag. Separate from strictParse
// because that one validates and discards; this runs after it has already
// proven the document well-formed, so a decode error here means the field is
// simply absent and the answer is the closed default.
func parseAllowHost(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var doc devstackDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return false
	}
	return doc.AllowHost
}

// devstackDoc is the strict shape we accept for `.sklp/dev.yaml`. It
// is intentionally narrower than pipeline.Pipeline (no `steps:`, no
// `tasks:`, etc.) so the strict decoder rejects fields that belong
// to other kinds.
type devstackDoc struct {
	Version     int                       `yaml:"version"`
	Kind        string                    `yaml:"kind"`
	Extends     pipeline.ExtendsList      `yaml:"extends,omitempty"`
	Name        string                    `yaml:"name,omitempty"`
	Description string                    `yaml:"description,omitempty"`
	Tools       map[string]string         `yaml:"tools,omitempty"`
	Env         map[string]string         `yaml:"env,omitempty"`
	EnvFiles    []string                  `yaml:"env_files,omitempty"`
	Services    map[string]devstackSvcDoc `yaml:"services,omitempty"`
	Jobs        map[string]devstackSvcDoc `yaml:"jobs,omitempty"`

	// AllowHost lets services reach the HOST's own LAN addresses, which the
	// space egress blackhole denies by default. It lives in THIS struct and not
	// in pipeline.Pipeline on purpose: the pipeline schema is shared with CI and
	// publish, and the exception must not be expressible there — a strict parse
	// of any non-devstack YAML rejects the field as unknown.
	AllowHost bool `yaml:"allowHost,omitempty"`
}

type devstackSvcDoc struct {
	Run       string            `yaml:"run,omitempty"`
	Image     string            `yaml:"image,omitempty"`
	Port      int               `yaml:"port,omitempty"`
	Ports     []string          `yaml:"ports,omitempty"`
	Volumes   []string          `yaml:"volumes,omitempty"`
	Command   []string          `yaml:"command,omitempty"`
	Workdir   string            `yaml:"workdir,omitempty"`
	Env       map[string]string `yaml:"env,omitempty"`
	DependsOn []string          `yaml:"depends_on,omitempty"`
	Optional  bool              `yaml:"optional,omitempty"`
	Oneshot   bool              `yaml:"oneshot,omitempty"`
	Ready     *devstackReadyDoc `yaml:"ready,omitempty"`
	Network   string            `yaml:"network,omitempty"`
}

type devstackReadyDoc struct {
	HTTP    string `yaml:"http,omitempty"`
	TCP     string `yaml:"tcp,omitempty"`
	Log     string `yaml:"log,omitempty"`
	File    string `yaml:"file,omitempty"`
	Timeout string `yaml:"timeout,omitempty"`
}

// strictIssue is one unknown-field error we lifted out of yaml.v3's
// concatenated error message. Path is the dotted YAML path where the
// problem occurred (e.g. "services.api.dependsOn"), Line is the 1-
// based source line, BadField is the rejected name, Suggestion is
// the closest known field (or empty if no good match), and Owner is
// the struct type the field would have belonged to ("service",
// "ready", "root") — used to look up the canonical field list.
type strictIssue struct {
	Line       int
	Path       string
	BadField   string
	Suggestion string
	Owner      string
}

// parseStrictErrors picks apart the textual error yaml.v3 produces in
// strict mode. Format example:
//
//	yaml: unmarshal errors:
//	  line 12: field dependsOn not found in type devstack.devstackSvcDoc
//
// We strip the type tail and resolve a high-level "owner" based on
// the type name so the error message stays user-facing.
func parseStrictErrors(msg string) []strictIssue {
	var out []strictIssue
	for _, line := range strings.Split(msg, "\n") {
		line = strings.TrimSpace(line)
		// Match: "line N: field X not found in type Y"
		const lineTag = "line "
		i := strings.Index(line, lineTag)
		if i < 0 {
			continue
		}
		rest := line[i+len(lineTag):]
		colon := strings.Index(rest, ":")
		if colon < 0 {
			continue
		}
		var lineNum int
		if _, err := fmt.Sscanf(rest[:colon], "%d", &lineNum); err != nil {
			continue
		}
		fieldTag := strings.Index(rest, "field ")
		notFound := strings.Index(rest, " not found in type ")
		if fieldTag < 0 || notFound < 0 || fieldTag > notFound {
			continue
		}
		bad := strings.TrimSpace(rest[fieldTag+len("field ") : notFound])
		typ := strings.TrimSpace(rest[notFound+len(" not found in type "):])
		owner := ownerFor(typ)
		issue := strictIssue{
			Line:     lineNum,
			BadField: bad,
			Owner:    owner,
		}
		issue.Suggestion = bestMatch(bad, knownFields(owner))
		out = append(out, issue)
	}
	return out
}

// ownerFor maps the Go type name leaked by yaml.v3 to a user-facing
// label so the error message never names internal types. Falls back
// to "service" when the type is unrecognised — that's the only nested
// shape with custom fields, so it's a safe default in practice.
func ownerFor(typ string) string {
	switch {
	case strings.HasSuffix(typ, "devstackDoc"):
		return "root"
	case strings.HasSuffix(typ, "devstackSvcDoc"):
		return "service"
	case strings.HasSuffix(typ, "devstackReadyDoc"):
		return "ready"
	default:
		return "service"
	}
}

// knownFields returns the YAML tag names declared on the strict
// schema for the given owner. Computed via reflection so adding a
// new field automatically extends the suggestion pool.
func knownFields(owner string) []string {
	var t reflect.Type
	switch owner {
	case "root":
		t = reflect.TypeOf(devstackDoc{})
	case "service":
		t = reflect.TypeOf(devstackSvcDoc{})
	case "ready":
		t = reflect.TypeOf(devstackReadyDoc{})
	default:
		return nil
	}
	var out []string
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("yaml")
		name := strings.TrimSuffix(strings.Split(tag, ",")[0], "")
		if name != "" && name != "-" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// bestMatch returns the closest known field by Levenshtein distance,
// or "" if no candidate is closer than half the bad-field length.
// The threshold prevents wild guesses (e.g. "foo" suggesting "run").
func bestMatch(bad string, candidates []string) string {
	if len(candidates) == 0 {
		return ""
	}
	bestName := ""
	bestDist := 1 << 30
	for _, c := range candidates {
		d := levenshtein(strings.ToLower(bad), strings.ToLower(c))
		if d < bestDist {
			bestDist = d
			bestName = c
		}
	}
	if bestDist > len(bad)/2+1 {
		return ""
	}
	return bestName
}

func levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min3(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[lb]
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}

// applyAutoFixes rewrites the source bytes by replacing each rejected
// field name with its suggestion, on the exact line yaml.v3 reported.
// Only applies fixes that have a non-empty Suggestion (i.e. passed
// the bestMatch confidence threshold).
//
// We operate line-by-line and only touch the leading `<key>:` token
// to avoid corrupting inline values that happen to contain the same
// substring (e.g. `run: echo dependsOn`).
func applyAutoFixes(raw []byte, issues []strictIssue) ([]byte, bool) {
	lines := bytes.Split(raw, []byte("\n"))
	any := false
	for _, iss := range issues {
		if iss.Suggestion == "" {
			continue
		}
		idx := iss.Line - 1
		if idx < 0 || idx >= len(lines) {
			continue
		}
		line := lines[idx]
		// Find the leading `<bad>:` token (after optional whitespace).
		trimmed := bytes.TrimLeft(line, " \t")
		prefix := line[:len(line)-len(trimmed)]
		if !bytes.HasPrefix(trimmed, []byte(iss.BadField+":")) {
			continue
		}
		rest := trimmed[len(iss.BadField):] // ":" + the rest
		newLine := append([]byte{}, prefix...)
		newLine = append(newLine, []byte(iss.Suggestion)...)
		newLine = append(newLine, rest...)
		lines[idx] = newLine
		any = true
	}
	if !any {
		return raw, false
	}
	return bytes.Join(lines, []byte("\n")), true
}

// summariseFixes produces a one-block diff summary for the user.
// Format: `  line 12: dependsOn → depends_on`.
func summariseFixes(issues []strictIssue) string {
	var b strings.Builder
	for _, iss := range issues {
		if iss.Suggestion == "" {
			continue
		}
		fmt.Fprintf(&b, "  line %d: %s → %s\n", iss.Line, iss.BadField, iss.Suggestion)
	}
	return b.String()
}

// formatIssues turns a slice of strictIssue into a multi-line user-
// facing error. Always shows the path, line, the offending field,
// and the list of valid fields. Adds a "Did you mean" hint when a
// suggestion exists.
func formatIssues(path string, raw []byte, issues []strictIssue) error {
	var b strings.Builder
	for i, iss := range issues {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "✗ %s:%d\n", path, iss.Line)
		fmt.Fprintf(&b, "    unknown field: %s\n", iss.BadField)
		if iss.Suggestion != "" {
			fmt.Fprintf(&b, "    did you mean: %s?\n", iss.Suggestion)
		}
		fields := knownFields(iss.Owner)
		if len(fields) > 0 {
			fmt.Fprintf(&b, "    valid fields for %s: %s\n", iss.Owner, strings.Join(fields, ", "))
		}
	}
	return fmt.Errorf("%s", b.String())
}

// friendlyParseError handles non-strict errors (genuine YAML syntax
// issues). yaml.v3 already reports line numbers, so we just prefix
// with the file path so editors / IDEs can jump to the location.
func friendlyParseError(path string, err error) error {
	return fmt.Errorf("✗ %s: %v", path, err)
}
