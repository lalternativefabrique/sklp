package sklpmcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/lalternativefabrique/sklp/internal/config"
)

const (
	redacted = "[redacted]"
	// Shorter values ("1", "true", a port) would blank out unrelated text.
	minSecretLen = 6
)

var authStatusRE = regexp.MustCompile(`status (401|403)\b`)

type redactor struct{ secrets []string }

// knownSecrets are the bearer tokens this machine holds, and the pinned
// fingerprint of the served context; no tool answer may carry them.
func knownSecrets(n *node) []string {
	var out []string
	if cfg, _, err := config.Load(); err == nil {
		out = append(out, config.EffectiveAPIToken(cfg.API))
		for _, c := range cfg.Contexts {
			out = append(out, config.EffectiveContextToken(c))
		}
	}
	if n != nil {
		out = append(out, n.token, n.pin, strings.ReplaceAll(strings.ToLower(n.pin), ":", ""))
	}
	return out
}

func (r *redactor) add(values ...string) {
	for _, v := range values {
		if len(v) >= minSecretLen {
			r.secrets = append(r.secrets, v)
		}
	}
}

func (r *redactor) str(s string) string {
	for _, sec := range r.secrets {
		s = strings.ReplaceAll(s, sec, redacted)
	}
	return s
}

func (r *redactor) err(err error) error {
	msg := err.Error()
	if m := authStatusRE.FindStringSubmatch(msg); m != nil {
		return fmt.Errorf("the node refused the context's credentials (status %s)", m[1])
	}
	if strings.HasPrefix(msg, "unauthorized") {
		return errors.New("the node refused the context's credentials (status 401)")
	}
	if strings.Contains(msg, "fingerprint mismatch") {
		return errors.New("the node's certificate does not match the fingerprint pinned in the context")
	}
	return errors.New(r.str(msg))
}

func scrub[T any](v T, r *redactor) (T, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return v, err
	}
	hit := false
	for _, sec := range r.secrets {
		quoted, _ := json.Marshal(sec)
		inner := quoted[1 : len(quoted)-1]
		if bytes.Contains(raw, inner) {
			raw = bytes.ReplaceAll(raw, inner, []byte(redacted))
			hit = true
		}
	}
	if !hit {
		return v, nil
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return v, err
	}
	return out, nil
}
