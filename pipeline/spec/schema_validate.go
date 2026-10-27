package spec

import (
	"fmt"
	"strings"
)

// validatePipeline checks invariants that span more than one field of
// the merged Pipeline. It runs AFTER `extends:` resolution so a value
// inherited from a parent template participates in the same checks as
// a value declared in the entry file.
//
// Current checks:
//
//   - Step.Services entries must reference a key present in
//     Pipeline.Services. Unknown names fail fast with a clear pointer to
//     the offending step + service name.
//   - Service.Volumes entries must use named volumes (`name:/path`).
//     Bind mounts (entries starting with "." or "/") are rejected: the
//     K8s sidecar materialiser cannot honour them, and silently dropping
//     them would surface as confusing runtime failures later.
//
// Validation runs unconditionally — the cost is O(steps + services) on a
// schema already parsed in memory.
func validatePipeline(p Pipeline) error {
	for _, step := range p.Steps {
		for _, name := range step.Services {
			if _, ok := p.Services[name]; !ok {
				return fmt.Errorf("step %q references unknown service %q", step.Name, name)
			}
		}
	}
	for name, svc := range p.Services {
		for _, v := range svc.Volumes {
			if isBindMount(v) {
				return fmt.Errorf("service %q volume %q: bind mounts are not supported; use a named volume (\"name:/path\")", name, v)
			}
		}
	}
	return nil
}

// isBindMount reports whether a Service.Volumes entry is a host-path
// bind mount rather than a named volume. The schema docs the named
// form as `name:/path[:ro]` and the bind form as `./data:/data` or
// `/abs/path:/data` — we look at the first segment before the first
// colon.
func isBindMount(v string) bool {
	if v == "" {
		return false
	}
	src := v
	if i := strings.IndexByte(v, ':'); i > 0 {
		src = v[:i]
	}
	return strings.HasPrefix(src, ".") || strings.HasPrefix(src, "/")
}
