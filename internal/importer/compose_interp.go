package importer

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// templateEnvFiles are committed dotenv templates: their values are already
// public, so the importer may inline them. `.env` itself is only read for its
// key names, never its values, since it usually holds local secrets.
var templateEnvFiles = []string{".env.example", ".env.sample", ".env.dist", ".env.template"}

type varSource struct {
	template     map[string]string
	templateFrom map[string]string
	local        map[string]bool
}

func loadVarSource(root string) *varSource {
	vs := &varSource{template: map[string]string{}, templateFrom: map[string]string{}, local: map[string]bool{}}
	for _, name := range templateEnvFiles {
		for k, v := range readDotenv(filepath.Join(root, name)) {
			if _, seen := vs.template[k]; !seen {
				vs.template[k] = v
				vs.templateFrom[k] = name
			}
		}
	}
	for k := range readDotenv(filepath.Join(root, ".env")) {
		vs.local[k] = true
	}
	return vs
}

func readDotenv(path string) map[string]string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	out := map[string]string{}
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := cutEq(line)
		if !ok || strings.TrimSpace(k) == "" {
			continue
		}
		out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return out
}

type expansion struct {
	value        string
	unresolved   []string
	fromTemplate []string
	defaulted    []string
	local        []string
}

func (e *expansion) resolved() bool { return len(e.unresolved) == 0 }

func (e *expansion) dedupe() {
	for _, p := range []*[]string{&e.unresolved, &e.fromTemplate, &e.defaulted, &e.local} {
		*p = uniqueSorted(*p)
	}
}

func (vs *varSource) expand(s string) expansion {
	var e expansion
	e.value = vs.expandInto(s, &e)
	e.dedupe()
	return e
}

func (vs *varSource) lookup(name string, e *expansion) (string, bool) {
	if vs.local[name] {
		e.local = append(e.local, name)
	}
	v, ok := vs.template[name]
	return v, ok
}

func (vs *varSource) expandInto(s string, e *expansion) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '$' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		next := s[i+1]
		switch {
		case next == '$':
			b.WriteByte('$')
			i++
		case next == '{':
			end := matchingBrace(s, i+1)
			if end < 0 {
				b.WriteString(s[i:])
				return b.String()
			}
			b.WriteString(vs.expandBraced(s[i:end+1], s[i+2:end], e))
			i = end
		case isNameStart(next):
			j := i + 1
			for j < len(s) && isNameChar(s[j]) {
				j++
			}
			name := s[i+1 : j]
			if v, ok := vs.lookup(name, e); ok {
				e.fromTemplate = append(e.fromTemplate, name)
				b.WriteString(v)
			} else {
				e.unresolved = append(e.unresolved, name)
				b.WriteString(s[i:j])
			}
			i = j - 1
		default:
			b.WriteByte('$')
		}
	}
	return b.String()
}

func (vs *varSource) expandBraced(raw, inner string, e *expansion) string {
	j := 0
	for j < len(inner) && isNameChar(inner[j]) {
		j++
	}
	name, rest := inner[:j], inner[j:]
	if name == "" {
		e.unresolved = append(e.unresolved, raw)
		return raw
	}
	op, word := "", ""
	for _, candidate := range []string{":-", ":?", ":+", "-", "?", "+"} {
		if strings.HasPrefix(rest, candidate) {
			op, word = candidate, rest[len(candidate):]
			break
		}
	}
	if op == "" && rest != "" {
		e.unresolved = append(e.unresolved, raw)
		return raw
	}
	v, set := vs.lookup(name, e)
	useValue := func() string {
		e.fromTemplate = append(e.fromTemplate, name)
		return v
	}
	switch op {
	case "":
		if set {
			return useValue()
		}
	case ":?", "?":
		if set && (op == "?" || v != "") {
			return useValue()
		}
	case ":-":
		if set && v != "" {
			return useValue()
		}
		e.defaulted = append(e.defaulted, name)
		return vs.expandInto(word, e)
	case "-":
		if set {
			return useValue()
		}
		e.defaulted = append(e.defaulted, name)
		return vs.expandInto(word, e)
	case ":+":
		if set && v != "" {
			return vs.expandInto(word, e)
		}
		return ""
	case "+":
		if set {
			return vs.expandInto(word, e)
		}
		return ""
	}
	e.unresolved = append(e.unresolved, name)
	return raw
}

// sameNameRef reports whether value is nothing but a reference to key itself
// (`${KEY}`, `$KEY`, `${KEY:-x}`, ...), and returns the default word if any.
func sameNameRef(key, value string) (def string, hasDefault, ok bool) {
	if value == "$"+key || value == "${"+key+"}" {
		return "", false, true
	}
	prefix := "${" + key
	if !strings.HasPrefix(value, prefix) || !strings.HasSuffix(value, "}") || matchingBrace(value, 1) != len(value)-1 {
		return "", false, false
	}
	rest := value[len(prefix) : len(value)-1]
	for _, op := range []string{":-", "-"} {
		if strings.HasPrefix(rest, op) {
			return rest[len(op):], true, true
		}
	}
	for _, op := range []string{":?", "?"} {
		if strings.HasPrefix(rest, op) {
			return "", false, true
		}
	}
	return "", false, false
}

func matchingBrace(s string, open int) int {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func isNameStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isNameChar(c byte) bool { return isNameStart(c) || (c >= '0' && c <= '9') }

func uniqueSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
