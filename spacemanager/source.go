package spacemanager

import "strings"

// ResolveSource resolves a manifest that never touched this machine: the raw
// deploy.yaml plus every ${VAR} value the sender resolved, so nothing is read
// from this process's env.
func ResolveSource(raw []byte, vars map[string]string, space, stack string) ([]Service, SpaceMeta, error) {
	lookup := func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	}
	cfg, err := ParseWith(raw, lookup)
	if err != nil {
		return nil, SpaceMeta{}, err
	}
	services, meta, err := cfg.Resolve(space, "")
	if err != nil {
		return nil, SpaceMeta{}, err
	}
	meta.Stack = stack
	meta.Registries = ResolveRegistryPasswordsWith(meta.Registries, lookup)
	return services, meta, nil
}

// SourceLiteralSecretKeys reports the sensitive-looking env keys a raw
// deploy.yaml writes as literals rather than ${VAR}.
func SourceLiteralSecretKeys(raw []byte, vars map[string]string) []string {
	cfg, err := ParseWith(raw, func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	})
	if err != nil {
		return nil
	}
	return cfg.LiteralSecretKeys()
}

// LiteralSecretsWarning explains why literal secrets end up in cleartext.
func LiteralSecretsWarning(keys []string) string {
	return strings.Join(keys, ", ") + " written as literal value(s) in the deploy file; " +
		"they are stored in cleartext in the manifest. Use ${VAR} to keep them out of it."
}
