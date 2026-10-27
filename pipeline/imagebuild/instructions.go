package imagebuild

import (
	"fmt"
	"strings"
)

// CopySpec is a parsed COPY instruction. From is the source stage name/index
// for `COPY --from=<stage>` (empty = copy from the host build context). Srcs are
// the source paths (relative to the context or the from-stage rootfs); Dst is
// the destination path in the current stage's rootfs.
type CopySpec struct {
	From  string   // stage ref for --from=, "" = host build context
	Chown string   // --chown=uid:gid (optional; "" = preserve)
	Srcs  []string // one or more source paths
	Dst   string   // destination path
}

// parseCopy parses `COPY [--from=X] [--chown=U:G] src... dst`. Docker allows
// multiple sources with the last token as the destination. Flags may appear in
// any order before the paths.
func ParseCopy(args string) (CopySpec, error) {
	var c CopySpec
	fields := strings.Fields(args)
	var paths []string
	for _, f := range fields {
		switch {
		case strings.HasPrefix(f, "--from="):
			c.From = strings.TrimPrefix(f, "--from=")
		case strings.HasPrefix(f, "--chown="):
			c.Chown = strings.TrimPrefix(f, "--chown=")
		case strings.HasPrefix(f, "--"):
			// Tolerate but ignore other COPY flags (--link, --chmod) for now;
			// they do not change WHICH bytes land, only metadata we can refine.
			if eq := strings.IndexByte(f, '='); eq < 0 {
				return CopySpec{}, fmt.Errorf("COPY: unsupported flag %q", f)
			}
		default:
			paths = append(paths, f)
		}
	}
	if len(paths) < 2 {
		return CopySpec{}, fmt.Errorf("COPY needs at least one source and a destination, got %q", args)
	}
	c.Srcs = paths[:len(paths)-1]
	c.Dst = paths[len(paths)-1]
	return c, nil
}

// RunSpec is a parsed RUN instruction: the shell command plus any
// `--mount=type=secret,id=…[,env=…|target=…]` mounts it requested. Only the
// secret mount type is supported (the one real Dockerfiles use for private
// fetches); other mount types are rejected.
type RunSpec struct {
	Cmd     string
	Secrets []SecretMount
}

// SecretMount is a RUN --mount=type=secret request. ID is the secret name the
// build was given; Target is where it appears inside the RUN (default
// /run/secrets/<id>).
type SecretMount struct {
	ID     string
	Target string
}

// parseRun parses a RUN body, peeling any leading `--mount=...` flags. The
// remainder is the shell command. Docker's --mount uses comma-separated
// key=value inside one token.
func ParseRun(args string) (RunSpec, error) {
	var r RunSpec
	rest := strings.TrimSpace(args)
	for strings.HasPrefix(rest, "--mount=") {
		// Take up to the next whitespace as the mount token.
		tok := rest
		if sp := strings.IndexAny(rest, " \t"); sp >= 0 {
			tok = rest[:sp]
			rest = strings.TrimSpace(rest[sp:])
		} else {
			rest = ""
		}
		m, err := parseMount(strings.TrimPrefix(tok, "--mount="))
		if err != nil {
			return RunSpec{}, err
		}
		r.Secrets = append(r.Secrets, m)
	}
	r.Cmd = rest
	return r, nil
}

func parseMount(spec string) (SecretMount, error) {
	var m SecretMount
	var typ string
	for _, kv := range strings.Split(spec, ",") {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "type":
			typ = v
		case "id":
			m.ID = v
		case "target", "dst":
			m.Target = v
		case "required", "mode", "uid", "gid":
			// tolerated, no effect on bytes
		}
	}
	if typ != "secret" {
		return SecretMount{}, fmt.Errorf("RUN --mount type %q not supported (only type=secret)", typ)
	}
	if m.ID == "" {
		return SecretMount{}, fmt.Errorf("RUN --mount=type=secret needs an id")
	}
	if m.Target == "" {
		m.Target = "/run/secrets/" + m.ID
	}
	return m, nil
}

// parseExec parses an ENTRYPOINT/CMD value, supporting both the JSON exec form
// (["a","b"]) and the shell form (a b). Returns the argv.
func ParseExec(args string) []string {
	s := strings.TrimSpace(args)
	if strings.HasPrefix(s, "[") {
		// crude JSON array parse: split on quotes; good enough for the measured
		// Dockerfiles (no escaped quotes inside argv).
		inner := strings.TrimSuffix(strings.TrimPrefix(s, "["), "]")
		var out []string
		for _, part := range strings.Split(inner, ",") {
			part = strings.TrimSpace(part)
			part = strings.Trim(part, `"`)
			if part != "" {
				out = append(out, part)
			}
		}
		return out
	}
	// Shell form: run via sh -c.
	return []string{"/bin/sh", "-c", s}
}

// EnvVar is one parsed KEY=value pair from an ENV (or LABEL) instruction.
type EnvVar struct{ Key, Value string }

// ParseEnv parses an ENV instruction's args into ordered KEY=value pairs,
// supporting both Docker forms:
//
//	ENV KEY=val KEY2="v 2" KEY3=val3   (multi-pair; values may be quoted)
//	ENV KEY rest of the line is value  (legacy single-pair, no '=')
//
// The legacy form is detected when the first whitespace-separated token has no
// '='; then everything after the key is a single value. The multi-pair form
// splits on whitespace between tokens, honouring double/single quotes around a
// value so `K="a b"` keeps the space. Surrounding quotes are stripped.
func ParseEnv(args string) []EnvVar {
	s := strings.TrimSpace(args)
	if s == "" {
		return nil
	}
	// Legacy `ENV KEY value...` form: first token has no '='.
	first := strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == '\t' })
	if len(first) > 0 && !strings.Contains(first[0], "=") {
		k := first[0]
		v := strings.TrimSpace(strings.TrimPrefix(s, k))
		return []EnvVar{{Key: k, Value: trimQuotes(v)}}
	}

	var out []EnvVar
	var key, val strings.Builder
	inKey := true
	var quote rune // active quote char, 0 = none
	flush := func() {
		if key.Len() > 0 {
			out = append(out, EnvVar{Key: key.String(), Value: trimQuotes(val.String())})
		}
		key.Reset()
		val.Reset()
		inKey = true
	}
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				val.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote = r
			inKey = false
		case (r == ' ' || r == '\t') && !inKey:
			flush()
		case (r == ' ' || r == '\t') && inKey:
			// whitespace before any '=' (between pairs) — skip.
		case r == '=' && inKey:
			inKey = false
		case inKey:
			key.WriteRune(r)
		default:
			val.WriteRune(r)
		}
	}
	flush()
	return out
}

// trimQuotes strips one layer of matching surrounding double or single quotes.
func trimQuotes(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}
