package domain

import (
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

// ConfigFileName sits beside a deploy.yaml and holds the values its ${VAR}
// references resolve against.
const ConfigFileName = "config.yaml"

// ConfigPathFor returns the config.yaml beside a manifest.
func ConfigPathFor(manifestPath string) string {
	return path.Join(path.Dir(manifestPath), ConfigFileName)
}

// ParsePlainConfig reads the values a config.yaml states in the clear.
//
// Sealed entries (`secure:`) are deliberately skipped: they are encrypted
// with a passphrase held in the operator's keyring, which this process does
// not have and must not have — holding the key to every secret would defeat
// the point of sealing them. A ${VAR} bound to one of those resolves through
// the project secret store instead, or not at all.
//
// A malformed file yields no values rather than an error: it costs a
// reconciliation the ${VAR} it could have resolved, which surfaces as a clear
// "references secret X" further down, rather than failing a deployment whose
// manifest may not reference config at all.
func ParsePlainConfig(raw []byte) map[string]string {
	var file struct {
		Config map[string]yaml.Node `yaml:"config"`
	}
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return nil
	}
	out := make(map[string]string, len(file.Config))
	for name, node := range file.Config {
		var s string
		// A scalar decodes into a string; a sealed entry is a mapping and
		// fails here, which is exactly the filter we want.
		if err := node.Decode(&s); err != nil {
			continue
		}
		if name = strings.TrimSpace(name); name != "" {
			out[name] = s
		}
	}
	return out
}
