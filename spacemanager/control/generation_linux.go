package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/lalternativefabrique/sklp/spacemanager"
)

// persistMu serialises every writer of a space's manifest and env: an apply
// persisting the generation it just launched, and a rollback re-persisting the
// one still serving.
var persistMu sync.Mutex

// PrevManifestPath is the manifest of the generation an apply replaced.
func PrevManifestPath(space string) string {
	return filepath.Join(spaceConfigDir(space), "manifest.prev.json")
}

// PrevSpaceEnvPath is the env of the generation an apply replaced.
func PrevSpaceEnvPath(space string) string {
	return filepath.Join(spaceConfigDir(space), "env.prev")
}

// snapshotGeneration keeps the manifest and env on disk as the previous
// generation before an apply overwrites them. A rollback needs both: the
// previous spec's env keys resolve through the previous refs and values, which
// an authoritative apply otherwise drops.
func snapshotGeneration(space string) error {
	if err := copyFile(ManifestPath(space), PrevManifestPath(space)); err != nil {
		return err
	}
	return copyFile(SpaceEnvPath(space), PrevSpaceEnvPath(space))
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.Remove(dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o600)
}

type generation struct {
	spec applySpec
	env  map[string]string
}

func readGeneration(manifestPath, envPath string) (generation, bool) {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return generation{}, false
	}
	var spec applySpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return generation{}, false
	}
	return generation{spec: spec, env: readEnvFile(envPath)}, true
}

func readEnvFile(path string) map[string]string {
	vars := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return vars
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if found && key != "" {
			vars[key] = value
		}
	}
	return vars
}

func (g generation) lookup(name string) (string, bool) {
	v, ok := g.env[name]
	return v, ok
}

// entryFor returns the persisted (stripped) entry of this generation that the
// live spec was launched from, when the live spec is exactly what this
// generation resolves to.
func (g generation) entryFor(live spacemanager.Service) (spacemanager.Service, bool) {
	for _, svc := range g.spec.Services {
		if svc.Name != live.Name {
			continue
		}
		resolved := spacemanager.ResolveEnvRefsWith([]spacemanager.Service{svc}, g.spec.Meta.EnvRefs, g.lookup)[0]
		if spacemanager.SameSpec(resolved, live) {
			return svc, true
		}
		return spacemanager.Service{}, false
	}
	return spacemanager.Service{}, false
}

// RepersistLive rewrites a space's manifest with the specs its supervisor is
// serving. After a rollback the manifest on disk names the rejected spec;
// without this a restart relaunches it with no previous generation to fall
// back to.
//
// Each live service is persisted with the refs and values of the generation
// it was launched from: the current one for a service the apply did move, the
// previous one for a service the rollback kept. Mixing the kept spec with the
// new refs left its secrets in clear on disk and made every later roll launch
// the new image with the old env.
func RepersistLive(space string, live []spacemanager.Service) error {
	persistMu.Lock()
	defer persistMu.Unlock()

	cur, ok := readGeneration(ManifestPath(space), SpaceEnvPath(space))
	if !ok {
		return fmt.Errorf("read manifest for space %q", space)
	}
	prev, hasPrev := readGeneration(PrevManifestPath(space), PrevSpaceEnvPath(space))

	refs := make(map[string]string, len(cur.spec.Meta.EnvRefs))
	for k, v := range cur.spec.Meta.EnvRefs {
		refs[k] = v
	}
	env := make(map[string]string, len(cur.env))
	for k, v := range cur.env {
		env[k] = v
	}
	declared := map[string]string{}
	rolled := map[string][]string{}
	for _, svc := range cur.spec.Services {
		declared[svc.Name] = svc.DeclaredImage
		rolled[svc.Name] = svc.RolledImages
	}

	out := make([]spacemanager.Service, 0, len(live))
	for _, svc := range live {
		entry, found := cur.entryFor(svc)
		if !found && hasPrev {
			if entry, found = prev.entryFor(svc); found {
				carryGenerationRefs(entry, prev, refs, env)
			}
		}
		if !found {
			entry = spacemanager.StripEnvRefs([]spacemanager.Service{svc}, unionRefs(cur, prev, hasPrev))[0]
		}
		if d := declared[svc.Name]; d != "" {
			entry.DeclaredImage = d
		}
		if r := rolled[svc.Name]; len(r) > 0 {
			entry.RolledImages = r
		}
		out = append(out, entry)
	}

	meta := cur.spec.Meta
	meta.EnvRefs = refs
	if err := WriteSpaceEnv(space, env); err != nil {
		return err
	}
	return SaveManifest(space, out, meta)
}

// carryGenerationRefs adds to refs and env what a kept entry needs to resolve
// at boot: the ref of each of its stripped keys and the values those refs
// name, taken from the generation it was launched from. The current
// generation wins on a key it also declares.
func carryGenerationRefs(entry spacemanager.Service, gen generation, refs, env map[string]string) {
	for _, e := range entry.Env {
		key, _, _ := strings.Cut(e, "=")
		ref, isRef := spacemanager.LookupEnvRef(gen.spec.Meta.EnvRefs, entry.Name, key)
		if !isRef {
			continue
		}
		// Carried under the same shape it was found: a scoped entry stays
		// scoped, or the key a sibling declares differently would silently
		// take this service's template.
		carryKey := key
		if _, scoped := gen.spec.Meta.EnvRefs[spacemanager.ScopedEnvKey(entry.Name, key)]; scoped {
			carryKey = spacemanager.ScopedEnvKey(entry.Name, key)
		}
		if _, taken := refs[carryKey]; !taken {
			refs[carryKey] = ref
		}
		names := spacemanager.TemplateVarNames(ref)
		if !spacemanager.IsEnvTemplate(ref) {
			names = []string{ref}
		}
		for _, name := range names {
			if _, has := env[name]; has {
				continue
			}
			if v, ok := gen.env[name]; ok {
				env[name] = v
			}
		}
	}
}

func unionRefs(cur, prev generation, hasPrev bool) map[string]string {
	out := map[string]string{}
	if hasPrev {
		for k, v := range prev.spec.Meta.EnvRefs {
			out[k] = v
		}
	}
	for k, v := range cur.spec.Meta.EnvRefs {
		out[k] = v
	}
	return out
}
