package spacemanager

import (
	"github.com/lalternativefabrique/sklp/internal/registry"
)

// LoginFileLookup wraps an env lookup so a registry password variable that
// the environment does not define is taken from the login file for its host:
// a laptop that ran `sklp registry login` deploys without exporting the
// secret. hostByVar maps the password variable to the registry host. A node
// or a CI run has no login file, so there the base lookup is all there is.
func LoginFileLookup(hostByVar map[string]string, base func(string) (string, bool)) func(string) (string, bool) {
	if len(hostByVar) == 0 {
		return base
	}
	var file *registry.File
	return func(name string) (string, bool) {
		if v, ok := base(name); ok {
			return v, true
		}
		host, isRegistryVar := hostByVar[name]
		if !isRegistryVar {
			return "", false
		}
		if file == nil {
			f, _, err := registry.Load()
			if err != nil {
				return "", false
			}
			file = f
		}
		r, err := file.Resolve(host)
		if err != nil || r.Password == "" {
			return "", false
		}
		return r.Password, true
	}
}

func hostByPasswordVar(varByHost map[string]string) map[string]string {
	out := make(map[string]string, len(varByHost))
	for host, v := range varByHost {
		out[v] = host
	}
	return out
}
