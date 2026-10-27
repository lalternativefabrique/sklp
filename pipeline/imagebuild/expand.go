package imagebuild

import "strings"

// Vars is the build-time variable table (ARG + ENV values in scope) used to
// expand ${VAR}/$VAR references in instruction arguments, Docker-style.
type Vars map[string]string

// Expand substitutes ${VAR}, $VAR, ${VAR:-default} and ${VAR:+alt} references in
// s using vars, mirroring Docker's build-time variable expansion. An unknown
// variable expands to the empty string (Docker behaviour). A literal "$$"
// escapes to "$". This is intentionally a small subset — the modifiers Docker
// actually documents for Dockerfile expansion — not a full shell parameter
// expansion.
func Expand(s string, vars Vars) string {
	return expand(s, vars, nil)
}

// ExpandKnown is Expand for instruction bodies, where a reference to a name the
// Dockerfile never declares must survive into the command text instead of
// collapsing to "". A RUN body is handed to a shell, so `arch="$(uname -m)" &&
// echo ${arch}` refers to a *shell* variable the builder cannot see; expanding
// it to "" silently corrupts the command. declared holds every name the
// Dockerfile knows (global + per-stage ARG, plus ENV); names in it expand
// normally — to "" when out of scope for this stage, which is Docker's
// semantics for an unredeclared global ARG — and names outside it are emitted
// verbatim for the shell to resolve.
func ExpandKnown(s string, vars Vars, declared map[string]bool) string {
	return expand(s, vars, declared)
}

func expand(s string, vars Vars, declared map[string]bool) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		c := s[i]
		if c != '$' {
			b.WriteByte(c)
			i++
			continue
		}
		// "$" at end of string: literal.
		if i+1 >= len(s) {
			b.WriteByte('$')
			i++
			continue
		}
		// "$$" -> literal "$".
		if s[i+1] == '$' {
			b.WriteByte('$')
			i += 2
			continue
		}
		if s[i+1] == '{' {
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				// Unterminated ${ : emit literally.
				b.WriteByte('$')
				i++
				continue
			}
			expr := s[i+2 : i+2+end]
			if v, ok := expandBraced(expr, vars, declared); ok {
				b.WriteString(v)
			} else {
				b.WriteString(s[i : i+2+end+1])
			}
			i += 2 + end + 1
			continue
		}
		// $NAME form: name is [A-Za-z_][A-Za-z0-9_]*.
		j := i + 1
		if !isNameStart(s[j]) {
			// "$" not starting a name: literal.
			b.WriteByte('$')
			i++
			continue
		}
		j++
		for j < len(s) && isNameChar(s[j]) {
			j++
		}
		if name := s[i+1 : j]; declared == nil || declared[name] {
			b.WriteString(vars[name])
		} else {
			b.WriteString(s[i:j])
		}
		i = j
	}
	return b.String()
}

// expandBraced handles the inside of ${...}: a bare name, name:-default, or
// name:+alt. ok is false when expr is a bare name the Dockerfile never declared,
// so the caller can re-emit the reference verbatim.
func expandBraced(expr string, vars Vars, declared map[string]bool) (string, bool) {
	if name, def, ok := strings.Cut(expr, ":-"); ok {
		if v, set := vars[name]; set && v != "" {
			return v, true
		}
		return expand(def, vars, declared), true
	}
	if name, alt, ok := strings.Cut(expr, ":+"); ok {
		if v, set := vars[name]; set && v != "" {
			return expand(alt, vars, declared), true
		}
		return "", true
	}
	if declared != nil && !declared[expr] {
		return "", false
	}
	return vars[expr], true
}

func isNameStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isNameChar(c byte) bool {
	return isNameStart(c) || (c >= '0' && c <= '9')
}

// ParseArg parses an `ARG` instruction's argument text into its name and
// optional default value. `ARG NAME` yields (NAME, "", false); `ARG NAME=val`
// yields (NAME, "val", true). The value may itself contain ${...} references,
// expanded by the caller against the current Vars.
func ParseArg(args string) (name, def string, hasDefault bool) {
	args = strings.TrimSpace(args)
	if n, v, ok := strings.Cut(args, "="); ok {
		return strings.TrimSpace(n), strings.Trim(strings.TrimSpace(v), `"`), true
	}
	return args, "", false
}
