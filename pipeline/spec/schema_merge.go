package spec

// mergePipeline returns dst with src overlaid on top following the
// documented composition rules:
//
//	scalars  → src wins when non-zero (Runner, Kind, Name, …)
//	maps     → key-by-key merge, src wins on conflict (Tools, Env)
//	lists    → concat + dedup by identity (Secrets, Cache.Path)
//	Steps    → src replaces dst entirely (a task owns its own pipeline)
//
// This function is the heart of `extends:` resolution. It is also the
// hook the spawner can later use to merge a project-level default
// pipeline on top of a workspace-level one without touching the YAML
// parser.
//
// dst is mutated in place AND returned, so the caller can write either
// `dst = mergePipeline(dst, src)` or rely on the in-place effect; tests
// favour the return-value style for clarity.
func mergePipeline(dst, src Pipeline) Pipeline {
	// Scalars — src wins when non-zero, otherwise keep dst.
	if src.Version != 0 {
		dst.Version = src.Version
	}
	if src.Kind != "" {
		dst.Kind = src.Kind
	}
	if src.Runner != "" {
		dst.Runner = src.Runner
	}
	if src.Name != "" {
		dst.Name = src.Name
	}
	if src.Description != "" {
		dst.Description = src.Description
	}

	// `extends:` itself is NOT inherited: each file declares its own
	// composition. Forgetting this would create surprising transitive
	// chains where _base extending _foo silently pulls _foo into every
	// descendant. We deliberately drop src.Extends here.

	// Maps — merge key by key, src wins.
	dst.Tools = mergeStringMap(dst.Tools, src.Tools)
	dst.Env = mergeStringMap(dst.Env, src.Env)
	dst.Inputs = mergeAnyMap(dst.Inputs, src.Inputs)
	dst.Targets = mergeAnyMap(dst.Targets, src.Targets)
	dst.ServiceMap = mergeAnyMap(dst.ServiceMap, src.ServiceMap)
	dst.Defaults = mergeAnyMap(dst.Defaults, src.Defaults)
	// Scopes: per-key replacement (NOT list concat). A child file that
	// redefines `backend: [...]` expects to override the inherited
	// glob list, not append to it — appending would silently grow the
	// matched fileset and surprise the user.
	dst.Scopes = mergeStringListMap(dst.Scopes, src.Scopes)

	// Lists — concat + dedup.
	dst.Secrets = dedupStrings(append(dst.Secrets, src.Secrets...))
	dst.Rules = dedupStrings(append(dst.Rules, src.Rules...))
	dst.Cache = mergeCache(dst.Cache, src.Cache)
	dst.EnvFiles = dedupStrings(append(dst.EnvFiles, src.EnvFiles...))

	// Services: per-key replacement, mirroring how Scopes merge. A
	// child file that redefines `services.api:` expects to override
	// the inherited definition wholesale, not field-merge it (that
	// would silently keep an inherited Ready probe the user thought
	// they removed).
	dst.Services = mergeServices(dst.Services, src.Services)

	// Steps and Tasks — replaced wholesale when src provides any.
	// Pipelines model `steps:` as the unit of work a file owns; a
	// template that wanted to share steps would inline them via the
	// caller, not via inheritance (keeps composition predictable).
	if len(src.Steps) > 0 {
		dst.Steps = src.Steps
	}
	if len(src.Tasks) > 0 {
		dst.Tasks = src.Tasks
	}

	return dst
}

func mergeStringMap(dst, src map[string]string) map[string]string {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		dst = make(map[string]string, len(src))
	}
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// mergeStringListMap is map merge for scope-style values where the
// value is a slice. The src entry replaces dst's value when the same
// key exists in both — see comment at the call site for the rationale.
func mergeStringListMap(dst, src map[string][]string) map[string][]string {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		dst = make(map[string][]string, len(src))
	}
	for k, v := range src {
		// Copy the slice so the caller can't mutate src out from under
		// us by appending to dst[k] later.
		clone := make([]string, len(v))
		copy(clone, v)
		dst[k] = clone
	}
	return dst
}

func mergeAnyMap(dst, src map[string]any) map[string]any {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		dst = make(map[string]any, len(src))
	}
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func dedupStrings(xs []string) []string {
	if len(xs) == 0 {
		return xs
	}
	seen := make(map[string]struct{}, len(xs))
	out := xs[:0]
	for _, x := range xs {
		if _, dup := seen[x]; dup {
			continue
		}
		seen[x] = struct{}{}
		out = append(out, x)
	}
	return out
}

func mergeServices(dst, src map[string]Service) map[string]Service {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		dst = make(map[string]Service, len(src))
	}
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// mergeCache concatenates dst+src and dedups entries by Path. The src
// entry wins when both define the same path (allows a task to override
// a template's cache key).
func mergeCache(dst, src []CacheEntry) []CacheEntry {
	if len(src) == 0 {
		return dst
	}
	byPath := make(map[string]int, len(dst)+len(src))
	merged := make([]CacheEntry, 0, len(dst)+len(src))
	for _, c := range dst {
		byPath[c.Path] = len(merged)
		merged = append(merged, c)
	}
	for _, c := range src {
		if i, ok := byPath[c.Path]; ok {
			merged[i] = c
			continue
		}
		byPath[c.Path] = len(merged)
		merged = append(merged, c)
	}
	return merged
}
