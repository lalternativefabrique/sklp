package control

import (
	"net/http"
	"strings"

	"github.com/lalternativefabrique/sklp/spacemanager"
)

// serveManifest answers a manifest read for one space. It is shared by the
// single-space mux and the multi-space dispatcher, which reaches it WITHOUT
// consulting the registry — see newMultiMux.
func serveManifest(w http.ResponseWriter, r *http.Request, space string) {
	if r.Method != http.MethodGet {
		http.Error(w, "manifest: GET required", http.StatusMethodNotAllowed)
		return
	}
	manifest, err := buildManifest(space)
	if err != nil {
		http.Error(w, "manifest: "+err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, manifest)
}

// buildManifest turns the persisted manifest of a space into its wire shape.
//
// It reads the manifest WITHOUT resolving env refs (ReadPersistedManifest, not
// LoadManifest): a stripped ${VAR} stays empty here and is reported as a
// reference, so serving this route can never hand out a secret the persist
// path deliberately moved to a 0600 file.
func buildManifest(space string) (ManifestResponse, error) {
	services, meta, err := ReadPersistedManifest(space)
	if err != nil {
		return ManifestResponse{}, err
	}

	name := meta.Name
	if name == "" {
		name = space
	}
	out := ManifestResponse{Space: name, Stack: meta.Stack, URL: meta.URL}
	for _, r := range meta.Routes {
		out.Routes = append(out.Routes, ManifestRoute{Service: r.Service, Host: r.Host, Path: r.Path})
	}
	for _, svc := range services {
		entry := manifestService(svc, meta.EnvRefs)
		if svc.Oneshot || svc.Schedule != "" {
			out.Jobs = append(out.Jobs, entry)
			continue
		}
		out.Services = append(out.Services, entry)
	}
	if out.Services == nil {
		out.Services = []ManifestService{}
	}
	return out, nil
}

func manifestService(svc spacemanager.Service, refs map[string]string) ManifestService {
	out := ManifestService{
		Name:      svc.Name,
		Image:     svc.Image,
		Command:   svc.Command,
		Replicas:  svc.Replicas,
		Port:      svc.Port,
		Ports:     svc.Ports,
		Volumes:   svc.Volumes,
		DependsOn: svc.DependsOn,
		Schedule:  svc.Schedule,
		Env:       manifestEnv(svc.Name, svc.Env, refs),
	}
	if svc.Resources.Any() {
		out.Resources = &ManifestResources{
			CPUCores:    svc.Resources.CPUCores,
			MemoryBytes: svc.Resources.MemoryBytes,
			PidsMax:     svc.Resources.PidsMax,
			Require:     svc.Resources.Require,
		}
	}
	if svc.Ready != nil {
		out.Ready = &ManifestReady{
			HTTP:    svc.Ready.HTTP,
			TCP:     svc.Ready.TCP,
			Log:     svc.Ready.Log,
			File:    svc.Ready.File,
			Timeout: svc.Ready.Timeout,
		}
	}
	if svc.Backup != nil {
		out.Backup = &ManifestBackup{Schedule: svc.Backup.Schedule, Command: svc.Backup.Command}
	}
	return out
}

// manifestEnv classifies each "KEY=value" entry. A key present in refs came
// from a ${VAR} and its value was stripped at persist time — it is reported by
// reference, never by value, even in the (impossible by construction, but
// cheap to guard) case where a value survived.
func manifestEnv(service string, env []string, refs map[string]string) []ManifestEnv {
	if len(env) == 0 {
		return nil
	}
	out := make([]ManifestEnv, 0, len(env))
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		if ref, isRef := spacemanager.LookupEnvRef(refs, service, key); isRef {
			out = append(out, referenceEnv(key, ref))
			continue
		}
		source := EnvSourceLiteral
		if spacemanager.SensitiveEnvKey(key) {
			source = EnvSourceSensitive
		}
		out = append(out, ManifestEnv{Key: key, Value: value, Source: source})
	}
	return out
}

// referenceEnv reports a templated entry: Ref names the vars behind it and
// Value carries the template — placeholders, never the secret — when the value
// was composed around them, so a reader still sees the shape of the URL.
func referenceEnv(key, ref string) ManifestEnv {
	if !spacemanager.IsEnvTemplate(ref) {
		return ManifestEnv{Key: key, Ref: ref, Source: EnvSourceReference}
	}
	names := spacemanager.TemplateVarNames(ref)
	entry := ManifestEnv{Key: key, Ref: strings.Join(names, ","), Source: EnvSourceReference}
	if len(names) != 1 || ref != "${"+names[0]+"}" {
		entry.Value = ref
	}
	return entry
}
