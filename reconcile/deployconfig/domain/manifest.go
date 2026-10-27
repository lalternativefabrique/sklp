package domain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// deployFile is what this context validates before a push reaches a node:
// the services' identity. The node parses the full file itself (see
// Manifest.Raw), so a field absent here is not dropped, only unchecked.
type deployFile struct {
	Services map[string]serviceDef `yaml:"services"`
	Jobs     map[string]serviceDef `yaml:"jobs"`
}

func (f deployFile) workloadImages() map[string]serviceDef {
	all := make(map[string]serviceDef, len(f.Services)+len(f.Jobs))
	for name, def := range f.Services {
		all["service/"+name] = def
	}
	for name, def := range f.Jobs {
		all["job/"+name] = def
	}
	return all
}

// serviceDef carries the fields validated here; ${VAR} in env is resolved so
// a missing secret is refused before anything is sent.
type serviceDef struct {
	Image    string            `yaml:"image"`
	Command  []string          `yaml:"command"`
	Env      map[string]string `yaml:"env"`
	Port     int               `yaml:"port"`
	Replicas *int              `yaml:"replicas"`
}

// Manifest is the parsed, validated result: the services ready to marshal
// into an apply spec (see apply_spec.go). Kept separate from deployFile so
// ParseManifest's caller never touches the raw YAML shape. Node, NodeID and
// Space are not read from the file: deploy-root.yaml is the only source of
// destination, and the reconcile handler fills them from its entry.
type Manifest struct {
	Node     string
	NodeID   string
	Space    string
	Services []ManifestService
	// Raw is the deploy.yaml exactly as committed; Vars holds every ${VAR} it
	// references, resolved. The node expands and parses Raw itself.
	Raw  []byte
	Vars map[string]string
}

// ManifestService is a validated service, kept for the deployment record.
type ManifestService struct {
	Name     string
	Image    string
	Command  []string
	Env      []string // sorted "KEY=value", matching spacemanager.envSlice's contract
	Port     int
	Replicas int
}

// Hash fingerprints the file as committed plus the resolved ${VAR} values,
// which is exactly what the node receives. Stored as
// Deployment.LastAppliedHash to tell "nothing changed" from "re-apply" on
// the next push, without persisting the manifest itself.
func (m *Manifest) Hash() string {
	sum := sha256.Sum256(mustMarshal(struct {
		Body string
		Vars map[string]string
	}{string(m.Raw), m.Vars}))
	return hex.EncodeToString(sum[:])
}

func referencedVars(raw []byte) []string {
	seen := map[string]bool{}
	var names []string
	for _, ref := range bracedVar.FindAllString(string(raw), -1) {
		name := ref[2 : len(ref)-1]
		if name == "SPACE" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// mustMarshal panics on a json.Marshal failure, which cannot happen for a
// string and a map[string]string.
func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("deployconfig: marshal manifest for hashing: %v", err))
	}
	return b
}

// serviceNameRE mirrors the shape apps/sklp/spacemanager/control/apply_linux.go
// enforces server-side (serviceNameRE there) — rejecting it here surfaces the
// error at reconcile time, in Skalpai's own log, instead of as an opaque 4xx
// from the node's control API.
var serviceNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// bracedVar matches the ${VAR} form, mirroring spacemanager's own bracedVar
// (apps/sklp/spacemanager/config.go) so both parsers reject the same shape.
var bracedVar = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*\}`)

// ErrSecretsNotSupported is returned when a deploy.yaml references ${VAR} in
// a service's env and ParseManifest was called with secrets == nil (no
// resolver wired). Historically this meant secret resolution for a
// Git-reconciled deploy.yaml was blocked outright on skalpai-88
// (feat/deploy-reads-project-secrets) — that landed (see issue 206ec5bd's
// step 4), so today this only fires when a caller genuinely has no
// SecretResolver to hand ParseManifest (e.g. a future path with no
// authenticated project/user context).
var ErrSecretsNotSupported = fmt.Errorf("%w: service env references ${VAR}, which this context cannot resolve yet", ErrValidation)

// ErrSecretNotFound is returned when a ${VAR} names a key SecretResolver did
// not return — either the secret does not exist in the project, or it
// exists but is not opted into AllowedRunners: ["deploy"] (project_secret's
// resolve endpoint does not distinguish the two, by design: see issue
// 206ec5bd's step-4 notes — a caller cannot use the error to enumerate a
// project's secret names it is not allowed to read).
var ErrSecretNotFound = fmt.Errorf("%w: referenced secret was not returned by the resolver (missing, or not opted into allowed_runners: [deploy])", ErrValidation)

// SecretResolver resolves a project's deploy-readable secrets — the
// domain's view of project_secret's resolve_secrets use case
// (apps/core/project_secret/application/resolve_secrets), called in-process
// rather than over HTTP (both bounded contexts run in the same core
// binary — see reconcile_deploy/handler.go's doc on why no PAT is needed).
// Returns {name: plaintext} for every secret the project has opted into
// AllowedRunners: ["deploy"]; a name absent from the map was either never
// declared or not opted in — ParseManifest cannot tell which, by the same
// design as project_secret's own HTTP endpoint.
type SecretResolver interface {
	ResolveSecrets(ctx context.Context, projectID, userID, reason string) (map[string]string, error)
}

// ParseManifest parses and validates a deploy.yaml's v1-subset bytes,
// resolving any ${VAR} in a service's env via secrets. raw is the file
// content fetched from Git at the pushed ref — this function never touches
// the filesystem or the process environment (unlike spacemanager.Load,
// which resolves ${VAR} from os.Environ() and reads sibling
// env_files/config.yaml — neither exists for a webhook-driven fetch; see
// manifest.go's package doc). secrets may be nil: a manifest with no ${VAR}
// parses identically either way, and one that DOES reference ${VAR} fails
// with ErrSecretsNotSupported rather than silently sending the literal
// "${VAR}" string to a container.
//
// secrets.ResolveSecrets is called AT MOST ONCE per ParseManifest call
// (lazily, only if some service's env actually references ${VAR}) — a
// deploy.yaml with no secrets never touches project_secret at all, and one
// that does pays for exactly one resolve regardless of how many ${VAR}
// references it has.
func ParseManifest(ctx context.Context, raw []byte, secrets SecretResolver, projectID, userID string) (*Manifest, error) {
	return ParseManifestWithConfig(ctx, raw, nil, secrets, projectID, userID)
}

// ParseManifestWithConfig is ParseManifest with the values a sibling
// config.yaml states in the clear. A ${VAR} resolves from the secret store
// first and falls back to those, so a name that is both stays a secret.
//
// Without this, a manifest referencing a plain value — a hostname, a model
// name, an internal URL — could not reconcile from Git at all: those live in
// config.yaml, which only the CLI reads, so core failed on the first one and
// declared it a missing secret.
func ParseManifestWithConfig(ctx context.Context, raw []byte, plain map[string]string, secrets SecretResolver, projectID, userID string) (*Manifest, error) {
	opts := ParseOptions{Plain: plain, ProjectID: projectID, UserID: userID}
	// A nil SecretResolver must stay a nil Resolver: boxing it would produce a
	// non-nil interface holding a nil value, and ParseManifestWith's "no
	// resolver available" check would never fire again.
	if secrets != nil {
		opts.Resolver = legacyResolver{secrets}
	}
	return ParseManifestWith(ctx, raw, opts)
}

// ParseOptions carries what resolving a manifest's ${VAR} needs. It exists so
// the secrets manifest could be added without a sixth positional argument, and
// so a caller that resolves nothing states that by leaving Resolver nil.
type ParseOptions struct {
	// Plain holds the values a sibling config.yaml states in the clear.
	Plain map[string]string
	// Resolver reads secrets. nil means none are available, and a manifest
	// referencing ${VAR} then fails with ErrSecretsNotSupported.
	Resolver Resolver
	// Secrets is the parsed secrets.yaml beside this manifest, nil when the
	// repo declares none — every ${VAR} then resolves from the Skalpai store,
	// which is what it did before manifests existed.
	Secrets   *SecretsManifest
	ProjectID string
	UserID    string
	// RegistryPassword is the ONLY source of a `registries:` password variable:
	// the stack's own registry credentials, the ones its image watches poll
	// with. Given the repository of an image the block's host serves, it
	// returns the secret set for its prefix. The project store and the plain
	// config are never consulted for it. nil means the stack has none.
	RegistryPassword func(ctx context.Context, imageRepo string) (string, bool)
}

// legacyResolver adapts the old whole-project SecretResolver to Resolver, so
// a caller still passing one keeps working while the port changes shape.
type legacyResolver struct{ inner SecretResolver }

func (l legacyResolver) Resolve(ctx context.Context, req SecretRequest) (map[string]string, error) {
	if l.inner == nil {
		return nil, nil
	}
	return l.inner.ResolveSecrets(ctx, req.ProjectID, req.UserID, req.Reason)
}

func ParseManifestWith(ctx context.Context, raw []byte, opts ParseOptions) (*Manifest, error) {
	plain, secrets := opts.Plain, opts.Resolver
	var file deployFile
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("parse deploy file: %w", err)
	}
	if len(file.Services) == 0 && len(file.Jobs) == 0 {
		return nil, fmt.Errorf("%w: deploy file declares no services or jobs", ErrValidation)
	}

	names := make([]string, 0, len(file.Services))
	for name := range file.Services {
		names = append(names, name)
	}
	sort.Strings(names)

	var resolved map[string]string // lazily fetched — see doc above

	registryHosts, err := registryPasswordVars(raw)
	if err != nil {
		return nil, err
	}
	registryVals := map[string]string{}
	registryValue := func(name string) (string, error) {
		if v, ok := registryVals[name]; ok {
			return v, nil
		}
		host := registryHosts[name]
		if opts.RegistryPassword != nil {
			if v, ok := stackRegistryPassword(ctx, opts.RegistryPassword, host, file.workloadImages()); ok {
				registryVals[name] = v
				return v, nil
			}
		}
		return "", fmt.Errorf("%w: registries %q: no stack registry credential covers its images (set one in the stack's Registry tab)", ErrSecretNotFound, host)
	}

	services := make([]ManifestService, 0, len(names))
	for _, name := range names {
		if !serviceNameRE.MatchString(name) {
			return nil, fmt.Errorf("%w: invalid service name %q: must match %s", ErrValidation, name, serviceNameRE.String())
		}
		def := file.Services[name]
		if def.Image == "" {
			return nil, fmt.Errorf("%w: service %q: image is required", ErrValidation, name)
		}
		// env is a COPY of def.Env (never the raw parsed map itself), written
		// into in place as ${VAR} entries resolve — a plain `env := def.Env`
		// would alias the same underlying map, and mutating it while
		// def.Env's own range (the loop below) is still iterating over it
		// would corrupt the very map being read.
		env := make(map[string]string, len(def.Env))
		for k, v := range def.Env {
			env[k] = v
		}
		for key, val := range def.Env {
			if !bracedVar.MatchString(val) {
				continue
			}
			needsStore := false
			for _, ref := range bracedVar.FindAllString(val, -1) {
				if _, isRegistry := registryHosts[ref[2:len(ref)-1]]; !isRegistry {
					needsStore = true
				}
			}
			if needsStore && secrets == nil && len(plain) == 0 {
				return nil, fmt.Errorf("%w: service %q env %q", ErrSecretsNotSupported, name, key)
			}
			if needsStore && resolved == nil && secrets != nil {
				var rerr error
				resolved, rerr = resolveDeclared(ctx, secrets, opts)
				if rerr != nil {
					return nil, rerr
				}
			}
			// A value can EMBED references rather than be one: DATABASE_URL is
			// "postgres://app:${POSTGRES_PASSWORD}@postgres:5432/app". Slicing
			// off the first two and last characters treated the whole string
			// as the name and looked up "stgres://app:${...}@...disabl".
			var missing string
			var registryErr error
			substituted := bracedVar.ReplaceAllStringFunc(val, func(ref string) string {
				// The secret store wins: a name in both is a secret whose
				// plain twin would otherwise silently override it.
				varName := ref[2 : len(ref)-1]
				if _, isRegistry := registryHosts[varName]; isRegistry {
					v, rerr := registryValue(varName)
					if rerr != nil && registryErr == nil {
						registryErr = rerr
					}
					return v
				}
				if v, ok := resolved[varName]; ok {
					return v
				}
				if v, ok := plain[varName]; ok {
					return v
				}
				if missing == "" {
					missing = varName
				}
				return ref
			})
			if registryErr != nil {
				return nil, registryErr
			}
			if missing != "" {
				return nil, fmt.Errorf("%w: service %q env %q references secret %q", ErrSecretNotFound, name, key, missing)
			}
			env[key] = substituted
		}
		replicas := 0
		if def.Replicas != nil {
			if *def.Replicas < 0 {
				return nil, fmt.Errorf("%w: service %q: replicas must be >= 0", ErrValidation, name)
			}
			replicas = *def.Replicas
		}
		services = append(services, ManifestService{
			Name:     name,
			Image:    def.Image,
			Command:  append([]string(nil), def.Command...),
			Env:      envSlice(env),
			Port:     def.Port,
			Replicas: replicas,
		})
	}

	varNames := referencedVars(raw)
	vars := make(map[string]string, len(varNames))
	for _, name := range varNames {
		if _, isRegistry := registryHosts[name]; isRegistry {
			v, rerr := registryValue(name)
			if rerr != nil {
				return nil, rerr
			}
			vars[name] = v
			continue
		}
		if secrets == nil && len(plain) == 0 {
			return nil, fmt.Errorf("%w: deploy file references ${%s}", ErrSecretsNotSupported, name)
		}
		if secrets != nil && resolved == nil {
			var rerr error
			resolved, rerr = resolveDeclared(ctx, secrets, opts)
			if rerr != nil {
				return nil, rerr
			}
		}
		if v, ok := resolved[name]; ok {
			vars[name] = v
			continue
		}
		if v, ok := plain[name]; ok {
			vars[name] = v
			continue
		}
		return nil, fmt.Errorf("%w: deploy file references secret %q", ErrSecretNotFound, name)
	}

	return &Manifest{Services: services, Raw: append([]byte(nil), raw...), Vars: vars}, nil
}

// envSlice converts a YAML env map into the sorted KEY=value slice
// spacemanager.Service.Env expects — mirrors spacemanager's own envSlice
// (apps/sklp/spacemanager/config.go) so the two parsers agree on shape, and
// so ManifestService's hash is deterministic across reconcile runs.
func envSlice(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// resolveDeclared asks the resolver for what the secrets manifest declares.
//
// With no manifest the Refs list is empty, which a resolver reads as "this
// project's deploy-readable secrets" — the question the port asked before it
// could ask a narrower one, and the reason a repo declaring nothing keeps
// reconciling unchanged.
func resolveDeclared(ctx context.Context, r Resolver, opts ParseOptions) (map[string]string, error) {
	req := SecretRequest{
		ProjectID: opts.ProjectID,
		UserID:    opts.UserID,
		Reason:    "deployconfig reconcile",
	}
	if opts.Secrets != nil {
		req.Stores = opts.Secrets.StoreByName()
		req.Refs = opts.Secrets.Secrets
	}
	resolved, err := r.Resolve(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("resolve secrets: %w", err)
	}
	return resolved, nil
}

// registryPasswordVars maps each `registries:` password variable to its host,
// for the entries written as a bare ${VAR}.
func registryPasswordVars(raw []byte) (map[string]string, error) {
	var probe struct {
		Registries map[string]struct {
			Password string `yaml:"password"`
		} `yaml:"registries"`
	}
	if err := yaml.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("parse deploy file (registries): %w", err)
	}
	out := map[string]string{}
	for host, r := range probe.Registries {
		if m := bracedVar.FindString(r.Password); m != "" && m == r.Password {
			out[m[2:len(m)-1]] = host
		}
	}
	return out, nil
}

// stackRegistryPassword asks the stack's credentials for the first service
// image the host serves. Credentials are keyed by image-repo prefix, so the
// image is what identifies the right one, not the bare host.
func stackRegistryPassword(ctx context.Context, lookup func(context.Context, string) (string, bool), host string, services map[string]serviceDef) (string, bool) {
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		repo := imageRepo(services[name].Image)
		if imageHost(repo) != host {
			continue
		}
		if v, ok := lookup(ctx, repo); ok {
			return v, true
		}
	}
	return "", false
}

func imageRepo(image string) string {
	if at := strings.Index(image, "@"); at >= 0 {
		image = image[:at]
	}
	slash := strings.LastIndex(image, "/")
	if colon := strings.LastIndex(image, ":"); colon > slash {
		image = image[:colon]
	}
	return image
}

func imageHost(repo string) string {
	slash := strings.Index(repo, "/")
	if slash < 0 {
		return ""
	}
	first := repo[:slash]
	if first == "localhost" || strings.ContainsAny(first, ".:") {
		return first
	}
	return ""
}
