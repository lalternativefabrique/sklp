package spacemanager

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/lalternativefabrique/sklp/ingress"
	"github.com/lalternativefabrique/sklp/space"
)

// ephemeralKey is the reserved space name whose entry is the MOULD for any
// space not explicitly listed under `spaces:` (e.g. a per-PR preview).
const ephemeralKey = "ephemeral"

// SpaceMeta is the resolved metadata of one space: enough for the CLI to
// report it, route it, and (later) enforce its TTL. It is the second return of
// Resolve; the first is the runnable []deploy.Service.
type SpaceMeta struct {
	// Name is the space name as requested (production, staging, pr-1234, …).
	Name string

	// Stack is the deploy stack that owns this space (the .sklp/<stack>/ the
	// manifest was resolved from). It is the ownership tag: a re-apply from a
	// DIFFERENT stack into a live space is refused (the second deploy would
	// otherwise reconcile away the first stack's services). Empty on manifests
	// written before this field existed — treated as "unknown owner", which
	// adopts rather than refuses (no false alarm on legacy spaces).
	Stack string

	// Ephemeral is true when the space was materialised from the ephemeral
	// mould (an unlisted name), false for a permanent named space.
	Ephemeral bool

	// AllowEgress lists the CIDRs this space's containers may reach on the
	// Internet. Empty leaves egress open, which is what every space did before
	// the field existed; a non-empty list makes outbound traffic default-deny.
	// East-west traffic on the space bridge is never affected.
	AllowEgress []string

	// Prefix is the instance/service-name prefix applied for isolation
	// ("<space>-"), or "" when the space shares the base namespace.
	Prefix string

	// URL is the resolved external URL (ephemeral mould `url:` with ${SPACE}
	// substituted), or "" when none was declared.
	URL string

	// TTL is the parsed lifetime of an ephemeral space; 0 = no TTL (permanent
	// spaces always have TTL 0).
	TTL time.Duration

	// Routes are the resolved public gateway routes of this space (from
	// spaces.<name>.gateway.routes). The host is carried here, not on the
	// service, so a service may have several routes and a host may be shared.
	// Empty when the space exposes nothing through the gateway.
	Routes []ResolvedRoute

	// GatewayMode is the front-door mode (singleton | per-space) the controller
	// honours: it travels in the persisted manifest so the detached supervisor
	// reads it at boot without the DeployConfig. It is a NODE-level property —
	// Resolve receives it as a parameter, never reads it from the parsed YAML —
	// set exclusively by `sklet agent --gateway-mode`.
	GatewayMode string

	// Registries are this space's private-registry pull credentials, keyed by
	// registry host. SECRET-SAFE: the manifest stores the username + the NAME of
	// the env var holding the password (PasswordEnv), never the password itself —
	// the detached supervisor re-resolves it from its own environment at boot, so
	// no secret is ever written to disk. Empty = anonymous pulls + docker config.
	Registries map[string]RegistryAuthMeta

	// EnvRefs maps an env KEY to the raw template its value was written as in
	// deploy.yaml ("${DB_URL}", "postgres://app:${PW}@db/app"). SECRET-SAFE,
	// same contract as Registries: the on-disk manifest carries the template,
	// and the supervisor rebuilds the value from the space's secrets at boot.
	// A key absent here had a literal value, which stays in the manifest
	// because nothing could re-supply it after a reboot. Manifests persisted
	// before templates map the key to a bare var NAME; ExpandTemplate reads
	// both.
	EnvRefs map[string]string

	// EnvValues holds every var the EnvRefs templates reference, by NAME.
	// Transport-only, like RegistryAuthMeta.Password: shipped to the node on a
	// remote apply, written to the space's 0600 env file, stripped before the
	// manifest is persisted.
	EnvValues map[string]string `json:",omitempty"`
}

// RegistryAuthMeta is one registry credential as carried in the manifest: the
// username and the env var holding the password (not the value). Resolved to a
// live credential at launch via ResolveRegistryAuths.
//
// Password is the RESOLVED value, populated ONLY for a remote apply: the client
// reads PasswordEnv from its own environment (the operator's .env) and ships the
// value to the supervisor over the TLS control API, because a detached agent has
// no access to the operator's env (os.Getenv would be empty). It is transport-
// only and lives in memory: both apply paths persist the manifest, and the
// remote one strips this field first (control.persistManifest), so the on-disk
// manifest stays secret-safe (name-only) either way. Empty in the local path
// (env is shared there).
type RegistryAuthMeta struct {
	Username    string
	PasswordEnv string
	Password    string `json:",omitempty"`
}

// ResolveRegistryPasswords returns a copy of metas with each Password filled
// from its PasswordEnv read in the CURRENT process environment. Used by the
// remote-apply client to capture the operator's secret before shipping the spec
// over the control API — the agent has no access to that env. A host whose env
// var is unset keeps an empty Password (the pull then fails loudly, the right
// signal). Nil-safe; leaves an already-set Password untouched.
func ResolveRegistryPasswords(metas map[string]RegistryAuthMeta) map[string]RegistryAuthMeta {
	hostByVar := make(map[string]string, len(metas))
	for host, m := range metas {
		if m.PasswordEnv != "" {
			hostByVar[m.PasswordEnv] = host
		}
	}
	return ResolveRegistryPasswordsWith(metas, LoginFileLookup(hostByVar, os.LookupEnv))
}

func ResolveRegistryPasswordsWith(metas map[string]RegistryAuthMeta, lookup func(string) (string, bool)) map[string]RegistryAuthMeta {
	if len(metas) == 0 {
		return metas
	}
	out := make(map[string]RegistryAuthMeta, len(metas))
	for host, m := range metas {
		if m.Password == "" && m.PasswordEnv != "" {
			m.Password, _ = lookup(m.PasswordEnv)
		}
		out[host] = m
	}
	return out
}

// registryAuthMeta builds the secret-safe per-host credential metadata for the
// manifest: the username plus the NAME of the env var the password referenced
// (captured by Parse before expansion). A registry whose password was a literal
// (not ${VAR}) gets an empty PasswordEnv — its value is then only available
// in-memory at deploy time, never persisted (a re-launch after reboot would need
// it re-supplied). Returns nil when no registries are declared.
func (c *DeployConfig) registryAuthMeta() map[string]RegistryAuthMeta {
	if len(c.Registries) == 0 {
		return nil
	}
	out := make(map[string]RegistryAuthMeta, len(c.Registries))
	for host, spec := range c.Registries {
		out[host] = RegistryAuthMeta{Username: spec.Username, PasswordEnv: c.registryPasswordEnv[host]}
	}
	return out
}

// ResolveRegistryAuths turns RegistryAuthMeta into live pull credentials. The
// password comes from (in order): the resolved Password field (a remote apply
// shipped it over the control API, because the agent can't read the operator's
// env), else PasswordEnv from the CURRENT process environment (the local path,
// where the supervisor shares the operator's env — and the reboot path, where
// the value is re-read from the host's own env). A host with neither a value nor
// a set env var is skipped (anonymous pull), which fails loudly at image resolve
// — the right signal.
func ResolveRegistryAuths(metas map[string]RegistryAuthMeta) map[string]space.RegistryAuth {
	return ResolveRegistryAuthsWith(metas, os.LookupEnv)
}

// ResolveRegistryAuthsWith reads a missing password from lookup — the space's
// own persisted secrets on a node, the process environment for a local run.
func ResolveRegistryAuthsWith(metas map[string]RegistryAuthMeta, lookup func(string) (string, bool)) map[string]space.RegistryAuth {
	if len(metas) == 0 {
		return nil
	}
	out := make(map[string]space.RegistryAuth, len(metas))
	for host, m := range metas {
		pw := m.Password
		if pw == "" && m.PasswordEnv != "" {
			pw, _ = lookup(m.PasswordEnv)
		}
		if m.Username == "" || pw == "" {
			continue
		}
		out[host] = space.RegistryAuth{Username: m.Username, Password: pw}
	}
	return out
}

// ResolvedRoute is one public route after config resolution: a service name (as
// it appears in the resolved service set, prefix already applied), the public
// host, and an optional path prefix. The agent turns each into a gateway
// Domain. Service is matched against the resolved service set.
type ResolvedRoute struct {
	Service string
	Host    string
	Path    string
	Rate    float64       // per-client-IP requests/sec; 0 = unlimited
	Burst   int           // token-bucket size; 0 = unlimited
	Timeout time.Duration // per-request cap (dial+response+body); 0 = unbounded
	MaxBody int64         // per-request body cap in bytes; 0 = unbounded

	// The behaviours below are what a public site needs beyond routing. They are
	// carried on the neutral model, not on one driver, so the same declaration
	// holds whether the front door is the native mux or traefik — a route that
	// behaved differently per driver would defeat the point of the model.
	RedirectApex    bool // 301 www.<host> -> <host>
	SecurityHeaders bool // HSTS, frameDeny, nosniff, referrer-policy
	Compress        bool // gzip text responses
	Passthrough     int  // TLS passthrough port; 0 = an HTTP route
}

// Resolve turns the config into the runnable service set + metadata for one
// space. It is a PURE function (config -> []Service + meta): no container,
// network, or filesystem, so the whole resolution is unit-tested directly.
//
// Resolution rules:
//   - A NAMED space (a key of `spaces:`, e.g. "production") layers its
//     per-service overrides onto the base services.
//   - An UNLISTED space (e.g. "pr-1234") is materialised from the `ephemeral:`
//     mould: mould-wide overrides applied, isolate:prefix renames every service
//     to "<space>-<svc>" (and rewrites depends_on accordingly), and ${SPACE}
//     in the url is substituted with the space name.
//   - An unlisted space with no `ephemeral:` mould is an error.
//
// gatewayMode is the NODE's own front-door mode (singleton | per-space), set
// exclusively by `sklet agent --gateway-mode`. It is a caller-supplied
// parameter, never read from the parsed YAML: a deploy.yaml must never
// influence the host's front door (see GatewaySpec).
func (c *DeployConfig) Resolve(space, gatewayMode string) ([]Service, SpaceMeta, error) {
	if space == "" {
		return nil, SpaceMeta{}, fmt.Errorf("space name is required")
	}

	spec, named := c.Spaces[space]
	if named {
		svcs, err := c.resolveNamed(spec)
		if err != nil {
			return nil, SpaceMeta{}, err
		}
		return svcs, SpaceMeta{Name: space, Routes: resolvedRoutes(spec), AllowEgress: normalizedAllowEgress(spec.AllowEgress), GatewayMode: gatewayMode, Registries: c.registryAuthMeta(), EnvRefs: c.envVarRef, EnvValues: c.envVarValues}, nil
	}

	// A config with NO `spaces:` block has no named spaces and no mould: every
	// space name resolves to the base set with no overrides (the single-space
	// common case). Only once `spaces:` is declared does an unlisted name
	// require the ephemeral mould to be materialised.
	if len(c.Spaces) == 0 {
		svcs, err := c.resolveNamed(SpaceSpec{})
		if err != nil {
			return nil, SpaceMeta{}, err
		}
		return svcs, SpaceMeta{Name: space, GatewayMode: gatewayMode, Registries: c.registryAuthMeta(), EnvRefs: c.envVarRef, EnvValues: c.envVarValues}, nil
	}

	mould, ok := c.Spaces[ephemeralKey]
	if !ok {
		return nil, SpaceMeta{}, fmt.Errorf(
			"space %q is not declared and no %q mould exists to materialise it",
			space, ephemeralKey)
	}
	return c.resolveEphemeral(space, gatewayMode, mould)
}

// resolveNamed builds the base service set with a named space's per-service
// overrides applied. Service order is deterministic (sorted) so the agent's
// observable order and any reporting is stable.
func (c *DeployConfig) resolveNamed(spec SpaceSpec) ([]Service, error) {
	out := make([]Service, 0, len(c.Services)+len(c.Jobs))
	for _, r := range c.runnables() {
		svc := r.spec.toRunnable(r.name, r.oneshot)
		if ov, ok := spec.Overrides[r.name]; ok {
			applyOverride(&svc, ov)
		}
		// The space's ingress is authoritative for host exposure: when set for
		// this service it OVERRIDES any service-level `ports:` (validate has
		// already checked it parses, matches expose, and is host-unique).
		if mapping, ok := spec.Ingress[r.name]; ok {
			svc.Ports = []string{mapping}
		}
		out = append(out, svc)
	}
	return out, nil
}

// resolvedRoutes turns a named space's gateway.routes into ResolvedRoute values.
// Named spaces apply no prefix, so the service name passes through unchanged.
// Returns nil when the space declares no gateway block.
func resolvedRoutes(spec SpaceSpec) []ResolvedRoute {
	if spec.Gateway == nil {
		return nil
	}
	out := make([]ResolvedRoute, 0, len(spec.Gateway.Routes))
	for _, r := range spec.Gateway.Routes {
		rate, burst := limitFor(r.Kind, r.Rate, r.Burst)
		timeout, _ := parseRouteTimeout(r.Timeout)
		maxBody, _ := parseRouteMaxBody(r.MaxBody)
		target := r.Service
		if r.Space != "" {
			target = r.Space + "/" + ingress.GatewayService
		}
		out = append(out, ResolvedRoute{
			MaxBody: maxBody,
			Service: target, Host: r.Host, Path: r.Path, Rate: rate, Burst: burst,
			Timeout:      timeout,
			RedirectApex: r.RedirectApex, SecurityHeaders: r.SecurityHeaders, Compress: r.Compress,
			Passthrough: r.Passthrough,
		})
	}
	return out
}

// resolveEphemeral materialises an unlisted space from the ephemeral mould:
// mould-wide replicas override, then (if isolate:prefix) the "<space>-" rename
// of every service name and its depends_on references, plus ${SPACE} in the
// url. The returned services are isolated from every other space by the
// prefix, so two previews never collide in the launcher/proxy.
func (c *DeployConfig) resolveEphemeral(space, gatewayMode string, mould SpaceSpec) ([]Service, SpaceMeta, error) {
	prefixed := mould.Isolate == "prefix"
	prefix := ""
	if prefixed {
		prefix = space + "-"
	}

	out := make([]Service, 0, len(c.Services)+len(c.Jobs))
	for _, r := range c.runnables() {
		svc := r.spec.toRunnable(prefix+r.name, r.oneshot)
		// isolate:prefix renames every node; rewrite each depends_on target to
		// the prefixed name too (services AND jobs), so an isolated space's
		// ordering graph stays internally consistent and never collides with
		// another space's nodes.
		if prefixed {
			for i, dep := range svc.DependsOn {
				// A cross-Space target ("space/service", ADR-007) names a real
				// Space elsewhere — prefixing it would silently rewrite it into
				// a nonexistent name instead of a same-space sibling.
				if strings.Contains(dep, "/") {
					continue
				}
				svc.DependsOn[i] = prefix + dep
			}
		}
		// Mould-wide overrides (replicas) apply to every service.
		if mould.Replicas != nil {
			svc.Replicas = *mould.Replicas
		}
		// Per-service overrides may also be carried in the mould entry.
		if ov, ok := mould.Overrides[r.name]; ok {
			applyOverride(&svc, ov)
		}
		out = append(out, svc)
	}

	meta := SpaceMeta{
		Name:        space,
		Ephemeral:   true,
		Prefix:      prefix,
		URL:         strings.ReplaceAll(mould.URL, "${"+spacePlaceholder+"}", space),
		AllowEgress: normalizedAllowEgress(mould.AllowEgress),
		GatewayMode: gatewayMode,
		Registries:  c.registryAuthMeta(),
		EnvRefs:     c.envVarRef,
		EnvValues:   c.envVarValues,
	}
	if mould.TTL != "" {
		d, err := time.ParseDuration(mould.TTL)
		if err != nil {
			return nil, SpaceMeta{}, fmt.Errorf("ephemeral mould ttl %q: %w", mould.TTL, err)
		}
		if d < 0 {
			return nil, SpaceMeta{}, fmt.Errorf("ephemeral mould ttl %q must be positive", mould.TTL)
		}
		meta.TTL = d
	}
	return out, meta, nil
}

// applyOverride patches the safe, space-scoped fields of a service. Only the
// fields explicitly set in the override are touched (pointer presence = "set").
// Env is merged key-by-key (override wins) rather than replaced wholesale.
func applyOverride(svc *Service, ov ServiceOverride) {
	if ov.Replicas != nil {
		svc.Replicas = *ov.Replicas
	}
	if ov.Port != nil {
		svc.Port = *ov.Port
	}
	if ov.Proxy != nil {
		// Proxy here is the bool "is this the proxy service"; the runnable
		// Service.Proxy is a routing rule string. A space override toggling
		// proxy is not meaningful for routing, so we ignore the value's effect
		// on the rule and keep it as a no-op placeholder for symmetry. (Left
		// minimal on purpose: proxy identity is a deploy-wide property.)
		_ = ov.Proxy
	}
	if len(ov.Env) > 0 {
		svc.Env = mergeEnv(svc.Env, ov.Env)
	}
}

// mergeEnv overlays override entries onto a sorted KEY=value base slice,
// override winning on key collision, and re-sorts for determinism.
func mergeEnv(base []string, over map[string]string) []string {
	m := make(map[string]string, len(base)+len(over))
	for _, kv := range base {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	for k, v := range over {
		m[k] = v
	}
	return envSlice(m)
}

// runnable is one entry of the services+jobs union, carrying the flag that
// resolution stamps onto the runnable Service (Oneshot for a job).
type runnable struct {
	name    string
	spec    ServiceSpec
	oneshot bool
}

// runnables returns the services+jobs union as runnable entries in a single
// deterministic (name-sorted) order, services and jobs interleaved. Resolution
// iterates this so a job lands in the runnable []Service exactly like a service,
// marked Oneshot. Names are unique across both maps (validate enforces it), so
// the merged sort is unambiguous.
func (c *DeployConfig) runnables() []runnable {
	out := make([]runnable, 0, len(c.Services)+len(c.Jobs))
	for n, s := range c.Services {
		out = append(out, runnable{name: n, spec: s, oneshot: false})
	}
	for n, s := range c.Jobs {
		out = append(out, runnable{name: n, spec: s, oneshot: true})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func (c *DeployConfig) sortedServiceNames() []string {
	names := make([]string, 0, len(c.Services))
	for n := range c.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// InstanceIDs returns the canonical steady-state instance ids of a service
// (<name>-0 .. <name>-N-1). It mirrors the ids the agent's reconcile loop
// assigns, so a separate `sklp deploy delete` process can Stop the same running
// instances without sharing agent state. Rollout "-v-" ids are NOT included
// (they only exist transiently mid-rollout); a delete between deploys hits the
// steady-state ids, which is the common case.
func InstanceIDs(svc Service) []string {
	n := svc.replicaCount()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		ids = append(ids, instanceID(svc.Name, i))
	}
	return ids
}

// ProxyServiceName returns the name of the service marked proxy: true, or "".
// The CLI uses it to wire the traefik proxy controller to the right service.
func (c *DeployConfig) ProxyServiceName() string {
	for _, name := range c.sortedServiceNames() {
		if c.Services[name].Proxy {
			return name
		}
	}
	return ""
}

// StripEnvRefs returns a copy of services with every env entry whose KEY is in
// refs reduced to the key alone (no value). The manifest then carries the name
// only; ResolveEnvRefs puts the value back at launch from the supervisor's own
// environment. Entries absent from refs are literals and are left untouched —
// nothing could re-supply them after a reboot.
//
// The copy is explicit: the caller's slice is still in use by the live
// supervisor, which needs the resolved values in memory.
func StripEnvRefs(services []Service, refs map[string]string) []Service {
	if len(refs) == 0 {
		return services
	}
	out := make([]Service, len(services))
	for i, svc := range services {
		if len(svc.Env) == 0 {
			out[i] = svc
			continue
		}
		env := make([]string, 0, len(svc.Env))
		for _, entry := range svc.Env {
			key, _, found := strings.Cut(entry, "=")
			if found {
				if _, isRef := LookupEnvRef(refs, svc.Name, key); isRef {
					env = append(env, key+"=")
					continue
				}
			}
			env = append(env, entry)
		}
		svc.Env = env
		out[i] = svc
	}
	return out
}

// envKeyScopeSep joins a service name to an env key when two services declare
// the same key with different templates. A unit separator cannot occur in
// either half, so the composite never collides with a real key, and a manifest
// without conflicts carries bare keys exactly as before.
const envKeyScopeSep = "\x1f"

// ScopedEnvKey names the per-service entry for a conflicting env key.
func ScopedEnvKey(service, key string) string {
	return service + envKeyScopeSep + key
}

// LookupEnvRef resolves an env key's template for one service: the scoped
// entry when the key was declared differently across services, the bare key
// otherwise. Reading both is what lets a manifest written before scoping
// resolve unchanged.
func LookupEnvRef(refs map[string]string, service, key string) (string, bool) {
	if service != "" {
		if ref, ok := refs[ScopedEnvKey(service, key)]; ok {
			return ref, true
		}
	}
	ref, ok := refs[key]
	return ref, ok
}

// ResolveEnvRefs fills every stripped env entry (KEY= with an empty value) whose
// key is in refs from the CURRENT process environment, reading the var NAME refs
// maps it to. This is the boot path: the supervisor's systemd unit loads the
// dotenv via EnvironmentFile=, so os.Getenv sees what the operator's machine saw
// at deploy time.
//
// A var that is unset on the host leaves the entry empty rather than dropping
// it, so the service fails on a missing value instead of silently starting with
// a different configuration.
func ResolveEnvRefs(services []Service, refs map[string]string) []Service {
	return ResolveEnvRefsWith(services, refs, os.LookupEnv)
}

// ResolveEnvRefsWith fills the referenced env values from lookup — the space's
// own persisted secrets on a node, the process environment for a local run.
func ResolveEnvRefsWith(services []Service, refs map[string]string, lookup func(string) (string, bool)) []Service {
	if len(refs) == 0 {
		return services
	}
	out := make([]Service, len(services))
	for i, svc := range services {
		if len(svc.Env) == 0 {
			out[i] = svc
			continue
		}
		env := make([]string, 0, len(svc.Env))
		for _, entry := range svc.Env {
			key, val, found := strings.Cut(entry, "=")
			if found && val == "" {
				if ref, isRef := LookupEnvRef(refs, svc.Name, key); isRef {
					env = append(env, key+"="+ExpandTemplate(ref, lookup))
					continue
				}
			}
			env = append(env, entry)
		}
		svc.Env = env
		out[i] = svc
	}
	return out
}

// normalizedAllowEgress canonicalises a space's allowlist for the runtime. It
// drops the error: validate() already rejected an unparsable entry, so by the
// time a space resolves, every entry parses.
func normalizedAllowEgress(entries []string) []string {
	out, err := normalizeAllowEgress(entries)
	if err != nil {
		return nil
	}
	return out
}

// splitMeshTarget parses a gateway route target: "<space>/<service>" names a
// service hosted on another node (routed via its mesh IP), a bare name a
// service of the local space. ok is false for a bare name.
func splitMeshTarget(target string) (space, service string, ok bool) {
	i := strings.IndexByte(target, '/')
	if i < 0 {
		return "", "", false
	}
	return target[:i], target[i+1:], true
}
