package spacemanager

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/lalternativefabrique/sklp/ingress"
	"github.com/lalternativefabrique/sklp/internal/stackconfig"
	"github.com/lalternativefabrique/sklp/internal/xdg"
	"github.com/lalternativefabrique/sklp/space"
	"gopkg.in/yaml.v3"
)

// DeployConfig is the parsed `.sklp/deploy.yaml`: the service definitions and
// the spaces that deploy them. It is the milestone-4 bridge from YAML to the
// milestone-1..3 reconciliation core (a []deploy.Service handed to the Agent).
//
// VOCABULARY (fixed): a `service` is a type of container; a `space` is an
// isolated group of services deployed/destroyed as one unit (permanent like
// production/staging, or ephemeral like a PR preview). Never "environment".
type DeployConfig struct {
	// Services are the base service definitions, keyed by service name. Every
	// space starts from this set and layers its overrides on top.
	Services map[string]ServiceSpec `yaml:"services"`

	// Jobs are run-once-to-completion tasks (migration, backup, one-off
	// maintenance), keyed by name and reusing the ServiceSpec shape. They are a
	// SEPARATE typed family from Services (k8s Deployment vs Job): a job resolves
	// to a Service with Oneshot=true. depends_on, validation and cycle detection
	// span the UNION of services+jobs; names must be unique across both maps.
	Jobs map[string]ServiceSpec `yaml:"jobs"`

	// EnvFiles are dotenv paths (relative to the deploy file's dir) whose keys
	// are loaded into the process environment BEFORE ${VAR} expansion, so a
	// deploy.yaml referencing ${S3_SECRET_KEY} et al. resolves without the
	// caller having to `source .env` first. Host env wins over file values. The
	// detached supervisor re-reads these same files via EnvironmentFile= in its
	// systemd unit, which is how a late-bound registry password survives the
	// hand-off to systemd (see RegistryAuthMeta).
	EnvFiles []string `yaml:"env_files"`

	// ProjectID, when set, is the Skalpai core project this deploy belongs to.
	// Load fetches every secret the project owner marked
	// AllowedRunners: ["deploy"] from GET /api/projects/:id/secrets/resolve and
	// loads them into the process env the same way EnvFiles does — so
	// ${VAR} resolves without the operator re-entering a value already saisi in
	// the web UI. Requires `sklp auth login`; a missing/expired token makes
	// this step a soft no-op (host env / env_files / stack config still apply),
	// since a deploy must not hard-fail just because the operator forgot to log
	// in and the secret is ALSO available another way.
	ProjectID string `yaml:"project_id,omitempty"`

	// Spaces are the named permanent spaces (production, staging, …) plus
	// the optional "ephemeral" entry, which is the MOULD applied to any space
	// name NOT listed here (e.g. a per-PR preview "pr-1234").
	Spaces map[string]SpaceSpec `yaml:"spaces"`

	// Gateway is the optional host-level front-door config. It selects the
	// gateway DRIVER (traefik by default) that `sklp gateway up` brings up to own
	// :443/:80 across all spaces. Absent = the default driver.
	Gateway *GatewaySpec `yaml:"gateway"`

	// Registries are pull credentials for private registries, keyed by registry
	// host (e.g. "rg.fr-par.scw.cloud"). This is the daemonless equivalent of a
	// Kubernetes imagePullSecret: the password is resolved from the environment
	// via ${VAR} at load time (never written in cleartext), and the runtime
	// prefers these over ~/.docker/config.json at pull time. Absent = anonymous
	// pulls + the on-disk docker config only.
	Registries map[string]RegistrySpec `yaml:"registries"`

	// registryPasswordEnv maps a registry host to the env var name its password
	// referenced in the raw YAML (the ${VAR} form), captured before expansion so
	// the secret can be persisted by NAME (RegistryAuthMeta.PasswordEnv) and
	// re-resolved at launch. Not a YAML field — populated by Parse.
	registryPasswordEnv map[string]string

	// envVarRef maps an env KEY to the raw TEMPLATE its value was written as
	// when that value references at least one ${VAR} ("${DB_URL}", or
	// "postgres://app:${PW}@db/app"), captured before expansion so the resolved
	// value is persisted as the template and rebuilt at launch. Not a YAML
	// field — set by Parse.
	envVarRef map[string]string

	// envVarValues holds the value of every var the templates in envVarRef
	// reference, keyed by var NAME. Transport-only: a remote apply ships them to
	// the node, which stores them in the space's 0600 env file and never in the
	// manifest. Not a YAML field — set by Parse.
	envVarValues map[string]string

	// resolvedEnvFiles are the EnvFiles as ABSOLUTE paths (relative entries
	// resolved against the deploy file's dir at Load). Not a YAML field — the
	// detached supervisor's systemd unit needs absolute EnvironmentFile= paths.
	resolvedEnvFiles []string
}

// EnvVarRefs returns the env KEY -> raw template map captured from the raw
// YAML. A key absent from the map had a literal value.
func (c *DeployConfig) EnvVarRefs() map[string]string { return c.envVarRef }

// ResolvedEnvFiles returns the absolute dotenv paths the supervisor's systemd
// unit should reference via EnvironmentFile=. Empty unless Load resolved them.
func (c *DeployConfig) ResolvedEnvFiles() []string { return c.resolvedEnvFiles }

// RegistrySpec is one registry's pull credential in a deploy.yaml `registries:`
// block. Username is typically a fixed value the registry mandates (Scaleway
// wants "nologin"); Password should be written as ${ENV_VAR} so the secret
// stays in the environment, not the YAML.
type RegistrySpec struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// GatewaySpec is the YAML shape of the top-level `gateway:` block: which driver
// realises the host front door. Kept minimal — the driver owns the rest.
//
// The front-door MODE (singleton | per-space) is deliberately NOT here: it is a
// node-level property owned exclusively by `sklet agent --gateway-mode`, never
// by a deploying app's deploy.yaml (see ValidateGatewayMode/ValidateNoLegacyMode
// for why — a deploy.yaml controlling it let whichever app deployed FIRST on an
// empty node silently decide the node's permanent front door).
type GatewaySpec struct {
	// Driver names the registered gateway driver (e.g. "traefik"). Empty =
	// DefaultGatewayDriver. Validated against the driver registry at parse time.
	Driver string `yaml:"driver"`

	// Email is the ACME (Let's Encrypt) contact for the traefik driver's TLS
	// certificates. REQUIRED for a real cert: Let's Encrypt rejects the
	// placeholder admin@example.com, so without this (or the
	// SKLP_GATEWAY_ACME_EMAIL env override) the gateway serves a self-signed
	// cert. Empty = fall back to the env var, then the placeholder.
	Email string `yaml:"email"`

	// ACME configures the front door (native per-space L7, or the traefik
	// singleton) to obtain real Let's Encrypt certs via the dns-01 challenge —
	// the only challenge that issues wildcards, and the only one the native L7
	// can answer behind its SNI-passthrough mux. Absent or disabled = the
	// native L7 serves per-SNI self-signed certs, and the traefik driver falls
	// back to httpChallenge. Reuses Email.
	ACME *ACMESpec `yaml:"acme,omitempty"`
}

// ACMESpec is the `gateway.acme:` block, shared by both gateway drivers: the
// native per-space L7 (certmagic) and the traefik singleton (its own
// lego-based dnsChallenge).
// It is node-wide: the DNS provider and account are shared; the domains under
// management are derived per-space from the route hosts. Secrets are late-bound
// — credentials name the ENV vars holding the DNS API keys, never the values.
type ACMESpec struct {
	// Enabled turns on ACME issuance for the native L7. Default false, so an
	// existing per-space deploy keeps serving self-signed until it opts in.
	Enabled bool `yaml:"enabled"`

	// Staging selects the Let's Encrypt STAGING CA (untrusted certs, relaxed rate
	// limits) — the staging→prod promotion knob. Flip to false for real certs.
	Staging bool `yaml:"staging"`

	// CA overrides the ACME directory URL outright (e.g. a private CA). Empty =
	// Let's Encrypt production (or staging when Staging is true).
	CA string `yaml:"ca,omitempty"`

	// DNSProvider names the registered dns-01 provider (e.g. "ovh"). Required
	// when Enabled. Validated against the provider registry at parse time.
	DNSProvider string `yaml:"dns_provider"`

	// Credentials maps the provider's credential keys to the ENV VAR NAMES that
	// hold their values (e.g. OVH_APPLICATION_KEY: OVH_APP_KEY). Resolved at load
	// time; only names live in YAML. When omitted, each credential key is read
	// from an env var of the same name.
	Credentials map[string]string `yaml:"credentials,omitempty"`

	// StorageDir is refused: where certmagic writes is the node's decision
	// (ACMEStorageDirEnv). Kept as a field so a manifest still naming it gets a
	// pointed error instead of a silently ignored key.
	StorageDir string `yaml:"storage_dir,omitempty"`

	// PropagationSeconds waits this long for the challenge TXT record to
	// propagate before asking the CA to validate. 0 = certmagic default.
	PropagationSeconds int `yaml:"propagation_seconds,omitempty"`
}

// ACMEStorageDirEnv lets the node operator place certmagic assets (account
// key + issued certs). It is a node property, never a manifest one: a manifest
// naming a host directory would hand a deploy an arbitrary write path.
const ACMEStorageDirEnv = "SKLP_ACME_STORAGE_DIR"

// ACMEStorageDir is the on-disk root for certmagic assets. It must be WRITABLE
// by the process: rootful sklp uses the FHS state path; rootless sklp (uid != 0,
// e.g. prod-shop) cannot create /var/lib, so it falls back to the per-user XDG
// state dir.
func ACMEStorageDir() string {
	if dir := os.Getenv(ACMEStorageDirEnv); dir != "" {
		return dir
	}
	if os.Geteuid() == 0 {
		return "/var/lib/sklp/acme"
	}
	return filepath.Join(xdg.StateRoot(), "acme")
}

// ovhCredentialKeys are the credential keys the OVH provider needs; used to
// default the env-name mapping when gateway.acme.credentials is omitted.
var acmeProviderCredKeys = map[string][]string{
	"ovh": {"OVH_ENDPOINT", "OVH_APPLICATION_KEY", "OVH_APPLICATION_SECRET", "OVH_CONSUMER_KEY"},
}

// ACMEEnabled reports whether native-L7 ACME issuance is configured and on.
func (c *DeployConfig) ACMEEnabled() bool {
	return c.Gateway != nil && c.Gateway.ACME != nil && c.Gateway.ACME.Enabled
}

// ResolvedACMEConfig builds the secrets-included ingress.ACMEConfig from the
// gateway.acme block, the gateway email, and the process env (credentials +
// SKLP_GATEWAY_ACME_EMAIL override). Returns ok=false when ACME is not enabled.
// The email precedence matches the traefik path: gateway.email, then the env
// override.
func (c *DeployConfig) ResolvedACMEConfig() (ingress.ACMEConfig, bool) {
	if !c.ACMEEnabled() {
		return ingress.ACMEConfig{}, false
	}
	a := c.Gateway.ACME
	email := c.GatewayEmail()
	if env := os.Getenv("SKLP_GATEWAY_ACME_EMAIL"); env != "" {
		email = env
	}
	return ingress.ACMEConfig{
		Email:              email,
		CADirURL:           a.CA,
		Staging:            a.Staging,
		StorageDir:         ACMEStorageDir(),
		DNSProvider:        a.DNSProvider,
		DNSCredentials:     a.ResolvedACMECredentials(),
		PropagationTimeout: time.Duration(a.PropagationSeconds) * time.Second,
	}, true
}

// ResolvedACMECredentials resolves the provider credential env-name mapping into
// actual values from the process env. When Credentials is omitted it reads each
// of the provider's credential keys from an env var of the same name. Values are
// returned by credential KEY (what the provider factory expects), not env name.
func (s *ACMESpec) ResolvedACMECredentials() map[string]string {
	out := map[string]string{}
	keys := acmeProviderCredKeys[s.DNSProvider]
	if len(s.Credentials) > 0 {
		for credKey, envName := range s.Credentials {
			out[credKey] = os.Getenv(envName)
		}
		return out
	}
	for _, k := range keys {
		out[k] = os.Getenv(k)
	}
	return out
}

// Gateway front-door modes. singleton = the legacy host traefik on :9080;
// per-space = an ingress container injected into each space, fronted by the host
// mux (`sklp gateway mux`). This is a NODE-level choice made exclusively by
// `sklet agent --gateway-mode` (mandatory, no default) — no deploy.yaml value
// backs it, so there is no cross-app default to keep here.
const (
	GatewayModeSingleton = "singleton"
	GatewayModePerSpace  = "per-space"
)

// ValidateGatewayMode rejects a front-door mode outside the known set. It is
// consumed by `sklet agent --gateway-mode` only: a deploy.yaml can no longer
// declare a mode (see GatewaySpec), so this is not exercised by the YAML parser
// itself.
func ValidateGatewayMode(mode string) error {
	switch mode {
	case GatewayModeSingleton, GatewayModePerSpace:
		return nil
	}
	return fmt.Errorf("unknown mode %q (known: %s, %s)", mode, GatewayModeSingleton, GatewayModePerSpace)
}

// GatewayDriverName returns the configured gateway driver, defaulting to
// DefaultGatewayDriver when the block (or its driver) is absent. It is the single
// place the CLI reads the driver selection from.
func (c *DeployConfig) GatewayDriverName() string {
	if c.Gateway == nil || c.Gateway.Driver == "" {
		return DefaultGatewayDriver
	}
	return c.Gateway.Driver
}

// GatewayEmail returns the ACME contact email configured under gateway.email,
// or "" when absent. The traefik driver reads it via SKLP_GATEWAY_ACME_EMAIL,
// which Parse exports from this field (env override still wins), so a deploy
// gets a real Let's Encrypt cert from the YAML alone.
func (c *DeployConfig) GatewayEmail() string {
	if c.Gateway == nil {
		return ""
	}
	return c.Gateway.Email
}

// ServiceSpec is the YAML shape of one service. It maps onto deploy.Service
// (image/command/env/port/replicas/proxy/ready) plus the carried-but-not-yet-
// reconciled fields (depends_on/volumes/ports) the runtime will honour later.
type ServiceSpec struct {
	Image     string            `yaml:"image"`
	Command   []string          `yaml:"command"`
	Replicas  *int              `yaml:"replicas"`
	Port      int               `yaml:"port,omitempty"`
	Proxy     bool              `yaml:"proxy"`
	Env       map[string]string `yaml:"env"`
	Ready     *ReadySpec        `yaml:"ready"`
	DependsOn []string          `yaml:"depends_on"`
	Volumes   []string          `yaml:"volumes"`
	Ports     []string          `yaml:"ports"`
	// ProxyProtocol lists the container ports (each must appear in Ports) whose
	// relayed connections the ingress prefixes with a PROXY protocol v2 header,
	// so the service sees the real client address instead of the space gateway.
	ProxyProtocol []int          `yaml:"proxy_protocol"`
	Backup        *BackupSpec    `yaml:"backup"`
	Resources     *ResourcesSpec `yaml:"resources"`
	// Capabilities the service needs BEYOND the hardened baseline, with or
	// without the CAP_ prefix. Rarely set: the baseline already covers an
	// entrypoint that chowns its data dir and drops to its own user.
	Capabilities []string `yaml:"capabilities"`

	// Schedule is the 5-field cron a `jobs:` entry re-runs on. Rejected on a
	// `services:` entry, which has no terminal state to re-run from.
	Schedule string `yaml:"schedule"`

	// Expose is the DEPRECATED alias for Port (a string port). It is normalised
	// into Port at parse time; new configs should use `port:`.
	Expose string `yaml:"expose,omitempty"`
}

// BackupSpec is the YAML `backup:` block. When present, BOTH fields are required
// (validated at parse): the command runs inside a live instance, the schedule
// (5-field cron) is when the supervisor fires it. Off-site/restore are external.
type BackupSpec struct {
	Command  []string `yaml:"command"`
	Schedule string   `yaml:"schedule"`
}

// ReadySpec is the YAML readiness probe. Exactly one of http/tcp/log/file is
// honoured (same precedence as space.ServiceReady). It maps 1:1 onto the
// runtime probe so the agent reuses the production probe backend.
type ReadySpec struct {
	HTTP    string `yaml:"http"`
	TCP     string `yaml:"tcp"`
	Log     string `yaml:"log"`
	File    string `yaml:"file"`
	Timeout string `yaml:"timeout"`
	Soak    string `yaml:"soak"`
}

func (r *ReadySpec) toProbe() *Probe {
	if r == nil {
		return nil
	}
	return &space.ServiceReady{
		HTTP:    r.HTTP,
		TCP:     r.TCP,
		Log:     r.Log,
		File:    r.File,
		Timeout: r.Timeout,
		Soak:    r.Soak,
	}
}

// SpaceSpec is the YAML shape of one space entry. A NAMED space
// (production/staging) carries per-service overrides in Overrides. The special
// "ephemeral" entry is the MOULD for unlisted spaces and additionally carries
// Isolate/URL/TTL plus mould-wide overrides (Replicas) inlined at its top level.
//
// We decode in two passes (see UnmarshalYAML) because the two shapes overlap:
//
//	production: { api: { replicas: 3 } }     # Overrides only
//	ephemeral:  { replicas: 1, isolate: ..., url: ..., ttl: 72h }
type SpaceSpec struct {
	// Overrides are per-service patches keyed by service name (named space).
	Overrides map[string]ServiceOverride

	// Ingress is the per-space host->space port map, keyed by service name with
	// a docker-style "host:container" value. The host port is a HOST resource the
	// space owns, so it lives here (not on the service) and two spaces may host-
	// expose the same service on different host ports without colliding (I6).
	Ingress map[string]string

	// Gateway is the per-space public routing block: a list of routes mapping a
	// service to a public host (+ optional path prefix). The host is carried by
	// the ROUTE, not the service, so one service may be exposed under several
	// hosts and several services may share a host on different paths. The shared
	// gateway (traefik) fronts these by Host/PathPrefix, with TLS for public
	// FQDNs. Mutually exclusive with Ingress per service (ingress = raw
	// host:port, no TLS; gateway = hostname routing + TLS).
	Gateway *SpaceGatewaySpec

	// AllowEgress lists the CIDRs this space's containers may reach on the
	// Internet. Absent leaves egress open (what every space did before this
	// field); a non-empty list makes outbound traffic default-deny, while
	// east-west traffic on the space bridge stays untouched.
	AllowEgress []string

	// Mould fields, only meaningful on the "ephemeral" entry.
	Isolate  string // "prefix" = prefix every service name with "<space>-".
	URL      string // routing URL template; "${SPACE}" is substituted.
	TTL      string // Go duration string; the space is torn down after it.
	Replicas *int   // mould-wide replica override applied to every service.
}

// SpaceGatewaySpec is the per-space `gateway:` block. For now it carries only
// the route list; future gateway-wide knobs (middlewares, tls options) live
// here too so the routing surface stays in one place.
type SpaceGatewaySpec struct {
	Routes []RouteSpec `yaml:"routes"`
}

// RouteSpec is one public route: a service exposed at a host, optionally scoped
// to a path prefix. Path empty = the host root. The pair (Host, Path) is unique
// across all spaces; the same Host with distinct Paths routes to distinct
// services (e.g. app.test → web, app.test/api → api).
type RouteSpec struct {
	// Service is the routed target: a service of THIS space by bare name, or a
	// service on ANOTHER NODE as "<space>/<service>" — the gateway then dials
	// the hosting node's mesh IP (resolved live from the peer cache) and the
	// remote Network Controller routes by internal host, so a public front can
	// sit on one machine while the workload runs WireGuard-only on another.
	Service string `yaml:"service"`
	// Space, instead of Service, hands the WHOLE host to a space on another
	// node: the gateway relays to that space's own ingress, which routes by
	// path from the space's own manifest (ADR-021). Path and the per-route
	// options below are refused on such a route; they belong to the space.
	Space string `yaml:"space,omitempty"`
	Host  string `yaml:"host"`
	Path  string `yaml:"path,omitempty"`

	// Kind declares what the route IS, so sklp can supply a sane rate limit
	// without the operator having to know the right numbers. It is the
	// alternative to guessing from the path: a login endpoint is just as often
	// /api/v2/sessions or /graphql as it is /login, so no name-matching
	// heuristic can find it — but its owner always knows what it is.
	// Empty = no default (unlimited unless Rate says otherwise).
	Kind string `yaml:"kind,omitempty"`

	// Rate rate-limits the route per client IP: RPS sustained requests/sec,
	// Burst the bucket size (max instantaneous). Both zero = unlimited. Both
	// front doors enforce it: the native L7 with a token bucket per IP, the
	// traefik driver via its rateLimit middleware (which averages over a
	// period rather than holding a bucket, so the shape differs slightly).
	// Set explicitly, they OVERRIDE whatever Kind would have supplied.
	Rate  float64 `yaml:"rate,omitempty"`
	Burst int     `yaml:"burst,omitempty"`

	// Timeout bounds ONE request end to end (backend dial, response, body) as
	// a Go duration string ("30s", "2m"). Empty or "0" = unbounded, which is
	// what a route serving large downloads, uploads or a WebSocket needs — so
	// it is opt-in per route rather than a global default that would be wrong
	// for one of those. It complements the L7's own idle deadline, which closes
	// a SILENT connection but by design never cuts one that keeps trickling
	// bytes (a slow-read client).
	Timeout string `yaml:"timeout,omitempty"`

	// MaxBody caps ONE request's body, in the same Ki/Mi/Gi vocabulary as
	// resources.memory ("1Mi", "10MB", or a plain byte count). Empty or "0" =
	// unbounded, and that is the default: unlike a rate limit, which only slows
	// a client down, a body cap is a flat refusal (413), so a value inherited
	// from anywhere would turn a working upload into an error nobody declared.
	// Deliberately NOT derived from Kind for that reason.
	MaxBody string `yaml:"max_body,omitempty"`

	// RedirectApex answers a "www." request with a 301 to the same URL on the
	// bare host. It is declared on the APEX route (host: example.com), not on a
	// www one: the redirect is a property of the domain, so the route that owns
	// example.com also owns where www.example.com goes.
	RedirectApex bool `yaml:"redirect_apex,omitempty"`

	// SecurityHeaders sets the response headers a public site is expected to
	// carry: HSTS, X-Frame-Options: DENY, X-Content-Type-Options: nosniff and a
	// referrer policy. Off by default — HSTS is hard to walk back, so it is
	// never turned on behind the operator's back.
	SecurityHeaders bool `yaml:"security_headers,omitempty"`

	// Compress gzips text responses (html/css/js/json/svg) above a size floor.
	Compress bool `yaml:"compress,omitempty"`

	// Passthrough routes the host's TLS connections by SNI to this port, never
	// decrypted by sklp: the target terminates TLS with its own certificate.
	Passthrough int `yaml:"passthrough,omitempty"`
}

// ServiceOverride is the subset of a service a space may patch. Only the
// safe, space-scoped knobs are overridable (replicas to scale a space,
// port/proxy/env to re-route or re-parameterise); image/command are intended
// to be uniform across spaces of one deploy.yaml and are NOT overridable here.
type ServiceOverride struct {
	Replicas *int              `yaml:"replicas"`
	Port     *int              `yaml:"port,omitempty"`
	Proxy    *bool             `yaml:"proxy"`
	Env      map[string]string `yaml:"env"`

	// Expose is the DEPRECATED alias for Port (a string port). It is normalised
	// into Port at parse time; new configs should use `port:`.
	Expose *string `yaml:"expose,omitempty"`
}

// spaceMouldKeys are the reserved keys of a space entry (mould knobs plus the
// per-space `ingress` map, valid on named spaces too). Any other key is treated
// as a per-service override.
var spaceMouldKeys = map[string]struct{}{
	"isolate": {}, "url": {}, "ttl": {}, "replicas": {}, "ingress": {}, "gateway": {},
	"allow_egress": {},
}

// UnmarshalYAML decodes a space entry, splitting the reserved mould keys
// (isolate/url/ttl/replicas) from the per-service overrides. This lets a named
// space (`{ api: {replicas: 3} }`) and the ephemeral mould (`{ replicas: 1,
// isolate: prefix, ... }`) share one YAML node shape without a tagged union.
func (c *SpaceSpec) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("space must be a mapping, got %s", nodeKind(value.Kind))
	}
	c.Overrides = map[string]ServiceOverride{}
	for i := 0; i+1 < len(value.Content); i += 2 {
		key := value.Content[i].Value
		val := value.Content[i+1]
		if _, reserved := spaceMouldKeys[key]; reserved {
			if err := c.decodeMouldKey(key, val); err != nil {
				return err
			}
			continue
		}
		var ov ServiceOverride
		if err := val.Decode(&ov); err != nil {
			return fmt.Errorf("space override %q: %w", key, err)
		}
		c.Overrides[key] = ov
	}
	return nil
}

func (c *SpaceSpec) decodeMouldKey(key string, val *yaml.Node) error {
	switch key {
	case "isolate":
		return val.Decode(&c.Isolate)
	case "url":
		return val.Decode(&c.URL)
	case "ttl":
		return val.Decode(&c.TTL)
	case "replicas":
		var n int
		if err := val.Decode(&n); err != nil {
			return fmt.Errorf("space replicas: %w", err)
		}
		c.Replicas = &n
	case "ingress":
		return val.Decode(&c.Ingress)
	case "gateway":
		var gw SpaceGatewaySpec
		if err := val.Decode(&gw); err != nil {
			return fmt.Errorf("space gateway: %w", err)
		}
		c.Gateway = &gw
	case "allow_egress":
		return val.Decode(&c.AllowEgress)
	}
	return nil
}

func nodeKind(k yaml.Kind) string {
	switch k {
	case yaml.ScalarNode:
		return "scalar"
	case yaml.SequenceNode:
		return "sequence"
	case yaml.MappingNode:
		return "mapping"
	default:
		return "value"
	}
}

// Load reads and parses a deploy.yaml from path, resolving ${VAR} from the
// process environment and validating the result. ${SPACE} is left intact at
// load time: it is a deploy-time placeholder substituted per space in Resolve.
func Load(path string) (*DeployConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read deploy file: %w", err)
	}
	// Load env_files into the process env BEFORE Parse expands ${VAR}, so a
	// deploy referencing ${S3_SECRET_KEY} et al. resolves without the caller
	// having to `source .env`. Relative paths resolve against the REPO ROOT (the
	// dir holding .sklp/), so the conventional `env_files: [.env]` finds the
	// .env at the repo root — not next to the deploy file two levels down.
	files, ferr := rawEnvFiles(raw)
	if ferr != nil {
		return nil, ferr
	}
	base := deployRepoRoot(path)
	if err := loadDeployEnvFiles(files, base); err != nil {
		return nil, err
	}
	// Load the Pulumi-style config.yaml files (plain values + secure: secrets
	// decrypted with SKLP_SECRETS_PASSPHRASE) into the process env, so ${VAR} in
	// the deploy file resolves from them — the committable, encrypted alternative
	// to a gitignored .env. Host env still wins, matching env_files semantics.
	if err := loadStackConfigs(base, filepath.Dir(path)); err != nil {
		return nil, err
	}
	cfg, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	// Record absolute env_file paths so the detached supervisor's unit can
	// EnvironmentFile= them (the CLI loaded them above; the supervisor re-reads).
	for _, f := range files {
		if !filepath.IsAbs(f) {
			f = filepath.Join(base, f)
		}
		cfg.resolvedEnvFiles = append(cfg.resolvedEnvFiles, f)
	}
	return cfg, nil
}

// deployRepoRoot resolves the repo root for a deploy file path. A deploy file
// normally lives at <root>/.sklp/<stack>/deploy.yaml, so the root is the dir
// containing the .sklp segment. Falls back to the deploy file's own dir for a
// path that isn't under a .sklp/ tree (e.g. an explicit -file elsewhere), which
// preserves the prior behaviour for that case.
func deployRepoRoot(path string) string {
	dir := filepath.Dir(path)
	// Walk up to the parent of the nearest ".sklp" segment.
	for d := dir; ; {
		if filepath.Base(d) == ".sklp" {
			return filepath.Dir(d)
		}
		parent := filepath.Dir(d)
		if parent == d {
			break // reached filesystem root, no .sklp found
		}
		d = parent
	}
	return dir
}

// rawEnvFiles extracts the env_files list from the un-expanded YAML, before
// ${VAR} expansion (env_files paths must not themselves depend on the env they
// load). Mirrors registryPasswordEnvs' raw pre-pass.
func rawEnvFiles(raw []byte) ([]string, error) {
	var probe struct {
		EnvFiles []string `yaml:"env_files"`
	}
	if err := yaml.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("parse deploy file (env_files): %w", err)
	}
	return probe.EnvFiles, nil
}

// loadDeployEnvFiles parses each dotenv file (relative to baseDir) and sets any
// key NOT already in the process env (host-wins). Missing files are skipped so
// an optional .env.local is harmless. Values are never logged.
func loadDeployEnvFiles(paths []string, baseDir string) error {
	for _, p := range paths {
		abs := p
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(baseDir, p)
		}
		f, err := os.Open(abs)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return fmt.Errorf("env_files: open %s: %w", p, err)
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			line = strings.TrimPrefix(line, "export ")
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			k = strings.TrimSpace(k)
			v = strings.Trim(strings.TrimSpace(v), `"'`)
			if k == "" {
				continue
			}
			if _, exists := os.LookupEnv(k); exists {
				continue // host env wins
			}
			if err := os.Setenv(k, v); err != nil {
				f.Close()
				return fmt.Errorf("env_files: setenv %s: %w", k, err)
			}
		}
		cerr := sc.Err()
		f.Close()
		if cerr != nil {
			return fmt.Errorf("env_files: scan %s: %w", p, cerr)
		}
	}
	return nil
}

// loadStackConfigs loads the repo-wide config.yaml then the stack's own, so a
// value shared by every stack is stored once at .sklp/config.yaml instead of
// being copied into each .sklp/<stack>/config.yaml. The order matches the CI
// loader (pipeline.stackConfigPaths): repo-wide first, the stack file second so
// it can specialise a shared value.
//
// A name the operator exported still wins over both, so the snapshot is taken
// before any injection: once the repo-wide file has Setenv'd a key, os.LookupEnv
// can no longer tell an operator export from this loader's own write, and the
// stack file could never override anything.
func loadStackConfigs(repoRoot, stackDir string) error {
	fromHost := make(map[string]bool, len(os.Environ()))
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 {
			fromHost[kv[:i]] = true
		}
	}
	paths := []string{filepath.Join(repoRoot, ".sklp", "config.yaml")}
	if stackConfig := filepath.Join(stackDir, "config.yaml"); stackConfig != paths[0] {
		paths = append(paths, stackConfig)
	}
	for _, path := range paths {
		if err := loadStackConfig(path, repoRoot, fromHost); err != nil {
			return err
		}
	}
	return nil
}

// loadStackConfig loads one config.yaml (plain values + secure: secrets
// decrypted with SKLP_SECRETS_PASSPHRASE) into the process env, so a later
// expandVars resolves ${VAR} from it. A name in fromHost is left untouched,
// like env_files. A missing file is a no-op; a wrong/absent passphrase for a
// file WITH secrets is a hard error (naming the offending key) so a deploy
// never silently drops a secret. Injected values are cleared from the deploy
// path's perspective by nothing — they persist in the process env, matching
// env_files behaviour, which is what the remote-apply resolution reads.
func loadStackConfig(path, repoRoot string, fromHost map[string]bool) error {
	c, err := stackconfig.Load(path)
	if err != nil {
		return fmt.Errorf("stack config: %w", err)
	}
	if len(c.Keys()) == 0 {
		return nil
	}
	pp, _, perr := stackconfig.ResolvePassphrase(repoRoot)
	if perr != nil {
		return fmt.Errorf("stack config %s: %w", path, perr)
	}
	resolved, err := c.Resolve(pp)
	if err != nil {
		return fmt.Errorf("stack config %s: %w", path, err)
	}
	for k, v := range resolved {
		if fromHost[k] {
			continue
		}
		if err := os.Setenv(k, v); err != nil {
			return fmt.Errorf("stack config: setenv %s: %w", k, err)
		}
	}
	return nil
}

// Parse parses deploy.yaml bytes: ${VAR} substitution, decode, validate.
func ParseWith(raw []byte, lookup func(string) (string, bool)) (*DeployConfig, error) {
	// Capture the registry password VAR NAMES from the raw (un-expanded) YAML
	// BEFORE expandVars rewrites them to values, so the secret stays late-bound:
	// the manifest persists the var name, and the password is resolved from the
	// environment only at launch. Everything else is expanded normally.
	rawPwEnv, err := registryPasswordEnvs(raw)
	if err != nil {
		return nil, err
	}
	rawEnvRefs, err := envVarRefs(raw)
	if err != nil {
		return nil, err
	}
	if err := rejectGatewayMode(raw); err != nil {
		return nil, err
	}

	lookup = LoginFileLookup(hostByPasswordVar(rawPwEnv), lookup)
	expanded, err := expandVarsWith(string(raw), lookup)
	if err != nil {
		return nil, err
	}
	var cfg DeployConfig
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parse deploy file: %w", err)
	}
	cfg.registryPasswordEnv = rawPwEnv
	cfg.envVarRef = rawEnvRefs
	cfg.envVarValues = templateVarValues(rawEnvRefs, lookup)
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func Parse(raw []byte) (*DeployConfig, error) {
	cfg, err := ParseWith(raw, os.LookupEnv)
	if err != nil {
		return nil, err
	}
	// Export gateway.email so the traefik driver (which reads
	// SKLP_GATEWAY_ACME_EMAIL) gets a real ACME contact from the YAML alone. An
	// existing env var wins, so an operator can still override per-host.
	if email := cfg.GatewayEmail(); email != "" {
		if _, set := os.LookupEnv("SKLP_GATEWAY_ACME_EMAIL"); !set {
			_ = os.Setenv("SKLP_GATEWAY_ACME_EMAIL", email)
		}
	}
	// Export the resolved native-L7 ACME config so every space worker (a re-exec
	// of this supervisor binary) inherits it via SKLP_SPACE_ACME and terminates
	// TLS with real certs. The value carries DNS credentials, so it lives only in
	// the process env, never logged. Crucially, a re-parse where ACME was REMOVED
	// must UNSET it: a long-lived supervisor (deploy agent) that re-applies a
	// config without ACME would otherwise keep minting certs with stale creds —
	// and leak one stack's ACME creds into another stack's workers.
	if acmeCfg, ok := cfg.ResolvedACMEConfig(); ok {
		if v, err := ingress.MarshalACMEEnv(acmeCfg); err == nil && v != "" {
			_ = os.Setenv(ingress.EnvACME, v)
		} else {
			_ = os.Unsetenv(ingress.EnvACME)
		}
	} else {
		_ = os.Unsetenv(ingress.EnvACME)
	}
	return cfg, nil
}

// registryPasswordEnvs parses the raw YAML to extract, per registry host, the
// env var name its password references (the ${VAR} form). Used to keep the
// secret late-bound: only the var name is persisted, never the value. A password
// that is not a single ${VAR} (e.g. a literal) yields "" for that host, which
// the launch resolver treats as a cleartext password carried in-memory only.
func registryPasswordEnvs(raw []byte) (map[string]string, error) {
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
		if m := bracedVar.FindStringSubmatch(r.Password); m != nil && r.Password == m[0] {
			out[host] = r.Password[2 : len(r.Password)-1] // strip ${ }
		}
	}
	return out, nil
}

// rejectGatewayMode fails a deploy.yaml that still declares `gateway.mode`.
// GatewaySpec dropped the field (front-door mode is now node-owned, set
// exclusively by `sklet agent --gateway-mode`): yaml.Unmarshal would otherwise
// silently drop the key, leaving a config that LOOKS like it selects a mode but
// has no effect — worse than a parse error, since nothing tells the operator.
func rejectGatewayMode(raw []byte) error {
	var probe struct {
		Gateway map[string]yaml.Node `yaml:"gateway"`
	}
	if err := yaml.Unmarshal(raw, &probe); err != nil {
		return fmt.Errorf("parse deploy file (gateway): %w", err)
	}
	if _, ok := probe.Gateway["mode"]; ok {
		return fmt.Errorf("gateway.mode is no longer a deploy.yaml field: the front-door mode is a node-level property set exclusively by `sklet agent --gateway-mode`; remove it from this file")
	}
	return nil
}

// normalize resolves the deprecated `expose:` string alias into the `port:` int
// across services, jobs and per-space overrides. `port:` wins when both are set
// (the alias is ignored); a non-numeric `expose:` is a hard error.
func (c *DeployConfig) normalize() error {
	for name, spec := range c.Services {
		if err := normalizeSpec("service", name, &spec); err != nil {
			return err
		}
		c.Services[name] = spec
	}
	for name, spec := range c.Jobs {
		if err := normalizeSpec("job", name, &spec); err != nil {
			return err
		}
		c.Jobs[name] = spec
	}
	for _, space := range c.Spaces {
		for name, ov := range space.Overrides {
			if err := normalizeOverride(name, &ov); err != nil {
				return err
			}
			space.Overrides[name] = ov
		}
	}
	return nil
}

// normalizeSpec folds spec.Expose into spec.Port. Port set = alias ignored and
// cleared; else a non-empty Expose must parse as a port.
func normalizeSpec(kind, name string, spec *ServiceSpec) error {
	if spec.Port == 0 && spec.Expose != "" {
		p, err := parsePort(spec.Expose)
		if err != nil {
			return fmt.Errorf("%s %q: expose: %w", kind, name, err)
		}
		spec.Port = p
	}
	spec.Expose = ""
	return nil
}

// normalizeOverride folds ov.Expose into ov.Port, same rules as normalizeSpec.
func normalizeOverride(name string, ov *ServiceOverride) error {
	if ov.Port == nil && ov.Expose != nil {
		p, err := parsePort(*ov.Expose)
		if err != nil {
			return fmt.Errorf("space override %q: expose: %w", name, err)
		}
		ov.Port = &p
	}
	ov.Expose = nil
	return nil
}

// spacePlaceholder is the one variable NOT resolved from the process env at
// load time: it is injected per space by Resolve (the space name).
const spacePlaceholder = "SPACE"

// bracedVar matches ONLY the ${VAR} form (braces required). A bare $VAR / $? /
// $2 / $(...) is left untouched so a service `command` that legitimately carries
// shell syntax passes through verbatim to the container's shell.
var bracedVar = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*\}`)

// expandVars resolves ${VAR} from the process environment. A missing variable
// is a hard error (deploy secrets must not silently fall back to empty), with
// the sole exception of ${SPACE}, which is a deploy-time placeholder kept
// verbatim for Resolve to fill in. Only the braced ${VAR} form is recognised
// (never the bare $VAR form), so command/url strings carrying a literal '$'
// — e.g. `$?`, `$2`, `$(date)` — survive untouched into the container shell.
func expandVars(s string) (string, error) {
	return expandVarsWith(s, os.LookupEnv)
}

func expandVarsWith(s string, lookup func(string) (string, bool)) (string, error) {
	var missing []string
	out := bracedVar.ReplaceAllStringFunc(s, func(match string) string {
		name := match[2 : len(match)-1] // strip "${" and "}"
		if name == spacePlaceholder {
			return "${" + name + "}"
		}
		val, ok := lookup(name)
		if !ok {
			missing = append(missing, name)
			return ""
		}
		return val
	})
	if len(missing) > 0 {
		sort.Strings(missing)
		return "", fmt.Errorf("deploy file references unset environment variable(s): %s", strings.Join(dedup(missing), ", "))
	}
	return out, nil
}

func dedup(in []string) []string {
	seen := map[string]struct{}{}
	out := in[:0]
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// TemplateVarNames returns the ${VAR} names a template references, sorted and
// without duplicates, ${SPACE} excluded. Empty for a literal.
func TemplateVarNames(template string) []string {
	var names []string
	for _, match := range bracedVar.FindAllString(template, -1) {
		name := match[2 : len(match)-1]
		if name == spacePlaceholder {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return dedup(names)
}

// IsEnvTemplate reports whether ref is a raw template rather than a bare var
// NAME — the form manifests persisted before templates carried.
func IsEnvTemplate(ref string) bool { return strings.Contains(ref, "${") }

// ExpandTemplate rebuilds an env value from its template with lookup. A var
// lookup cannot supply expands to empty, so the service fails on a missing
// value instead of starting with a different configuration; a bare var NAME
// (a manifest persisted before templates) is looked up as-is.
func ExpandTemplate(ref string, lookup func(string) (string, bool)) string {
	if !IsEnvTemplate(ref) {
		v, _ := lookup(ref)
		return v
	}
	return bracedVar.ReplaceAllStringFunc(ref, func(match string) string {
		name := match[2 : len(match)-1]
		if name == spacePlaceholder {
			return match
		}
		v, _ := lookup(name)
		return v
	})
}

func templateVarValues(refs map[string]string, lookup func(string) (string, bool)) map[string]string {
	out := map[string]string{}
	for _, template := range refs {
		for _, name := range TemplateVarNames(template) {
			if v, ok := lookup(name); ok {
				out[name] = v
			}
		}
	}
	return out
}

// envSlice converts a YAML env map into the sorted KEY=value slice the agent
// and launcher consume. Sorted so the runnable spec is deterministic (the agent
// compares Env to detect a spec change that warrants a rolling update).
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

// toService maps a ServiceSpec onto the runnable deploy.Service. proxy=true
// marks the reverse-proxy service; it has no Port of its own (it IS the front
// door), so its routing fields stay empty here.
func (s ServiceSpec) toService(name string) Service {
	return s.toRunnable(name, false)
}

// toRunnable is the shared ServiceSpec -> Service mapping; oneshot distinguishes
// a job (run-once) from a long-lived service. DependsOn is append-copied (like
// Volumes/Ports) so a later prefix rewrite mutates the copy, not the spec.
func (s ServiceSpec) toRunnable(name string, oneshot bool) Service {
	svc := Service{
		Name:          name,
		Image:         s.Image,
		Command:       append([]string(nil), s.Command...),
		Env:           envSlice(s.Env),
		Port:          s.Port,
		Ready:         s.Ready.toProbe(),
		Ports:         append([]string(nil), s.Ports...),
		ProxyProtocol: append([]int(nil), s.ProxyProtocol...),
		Volumes:       append([]string(nil), s.Volumes...),
		Capabilities:  normalizedCapabilities(s.Capabilities),
		DependsOn:     append([]string(nil), s.DependsOn...),
		Oneshot:       oneshot,
		Schedule:      s.Schedule,
	}
	if s.Replicas != nil {
		svc.Replicas = *s.Replicas
	}
	if s.Backup != nil {
		svc.Backup = &ServiceBackup{
			Command:  append([]string(nil), s.Backup.Command...),
			Schedule: s.Backup.Schedule,
		}
	}
	if res, err := s.Resources.parse(); err == nil && res != nil {
		svc.Resources = *res
	}
	ApplyDefaultResources(&svc.Resources)
	return svc
}

// validate enforces the structural invariants the agent assumes: at most one
// proxy service, every depends_on target exists, and no dependency cycle.
func (c *DeployConfig) validate() error {
	if len(c.Services) == 0 && len(c.Jobs) == 0 {
		return fmt.Errorf("deploy file declares no services or jobs")
	}
	// A name must be unique across the services+jobs union: depends_on resolves
	// over that union, so a collision is ambiguous.
	for name := range c.Jobs {
		if _, ok := c.Services[name]; ok {
			return fmt.Errorf("name %q is declared as both a service and a job; names must be unique across services and jobs", name)
		}
	}
	proxies := 0
	for name, spec := range c.Services {
		if spec.Image == "" {
			return fmt.Errorf("service %q: image is required", name)
		}
		if spec.Proxy {
			proxies++
		}
		if spec.Replicas != nil && *spec.Replicas < 0 {
			return fmt.Errorf("service %q: replicas must be >= 0", name)
		}
		maps, err := parsePortMappings(spec.Ports)
		if err != nil {
			return fmt.Errorf("service %q: %w", name, err)
		}
		if _, err := applyProxyProtocol(maps, spec.ProxyProtocol); err != nil {
			return fmt.Errorf("service %q: %w", name, err)
		}
		if err := c.validateDependsOn("service", name, spec.DependsOn); err != nil {
			return err
		}
		if err := validateBackup("service", name, spec.Backup); err != nil {
			return err
		}
		if err := validateCapabilities("service", name, spec.Capabilities); err != nil {
			return err
		}
		if err := validateVolumes("service", name, spec.Volumes); err != nil {
			return err
		}
		if _, err := spec.Resources.parse(); err != nil {
			return fmt.Errorf("service %q: resources: %w", name, err)
		}
		if spec.Schedule != "" {
			return fmt.Errorf("service %q: schedule is only valid on a job (a service has no terminal state to re-run from); move it under jobs:", name)
		}
	}
	if proxies > 1 {
		return fmt.Errorf("at most one service may set proxy: true, found %d", proxies)
	}
	// Reject an unparsable allowlist at parse time: discovering it when the
	// space starts would leave a space believing it filters what it does not.
	for name, spec := range c.Spaces {
		if _, err := normalizeAllowEgress(spec.AllowEgress); err != nil {
			return fmt.Errorf("space %q: %w", name, err)
		}
	}
	for name, spec := range c.Jobs {
		if spec.Image == "" {
			return fmt.Errorf("job %q: image is required", name)
		}
		if _, err := spec.Resources.parse(); err != nil {
			return fmt.Errorf("job %q: resources: %w", name, err)
		}
		if _, err := parsePortMappings(spec.Ports); err != nil {
			return fmt.Errorf("job %q: %w", name, err)
		}
		if err := c.validateDependsOn("job", name, spec.DependsOn); err != nil {
			return err
		}
		if err := validateBackup("job", name, spec.Backup); err != nil {
			return err
		}
		if err := validateVolumes("job", name, spec.Volumes); err != nil {
			return err
		}
		if spec.Schedule != "" {
			if _, err := ParseCron(spec.Schedule); err != nil {
				return fmt.Errorf("job %q: schedule %w", name, err)
			}
		}
	}
	if cycle := c.findCycle(); cycle != "" {
		return fmt.Errorf("depends_on cycle detected: %s", cycle)
	}
	if err := c.validateIngress(); err != nil {
		return err
	}
	if err := c.validateGateway(); err != nil {
		return err
	}
	if c.Gateway != nil && c.Gateway.Driver != "" && !KnownGatewayDriver(c.Gateway.Driver) {
		return fmt.Errorf("gateway: unknown driver %q (known: %s)", c.Gateway.Driver, knownGatewayDriverList())
	}
	if err := c.validateACME(); err != nil {
		return err
	}
	if err := c.validateRegistries(); err != nil {
		return err
	}
	return nil
}

// validateACME checks the gateway.acme block when present and enabled: a known
// dns-01 provider and a real ACME contact (gateway.email or the env override).
// A disabled/absent block is always valid. Both the native per-space L7
// (certmagic) and the traefik singleton (its own lego-based dnsChallenge)
// consume this same block — see validateGatewayModeACME in space.go, which no
// longer restricts it to per-space mode now that traefik solves dns-01 too.
func (c *DeployConfig) validateACME() error {
	if c.Gateway == nil || c.Gateway.ACME == nil || !c.Gateway.ACME.Enabled {
		return nil
	}
	a := c.Gateway.ACME
	if a.StorageDir != "" {
		return fmt.Errorf("gateway.acme.storage_dir is not a manifest setting: a deploy cannot pick a host directory to write into; set %s in the node's environment", ACMEStorageDirEnv)
	}
	if a.DNSProvider == "" {
		return fmt.Errorf("gateway.acme.dns_provider is required when acme is enabled (known: %v)", ingress.KnownDNSProviders())
	}
	if !ingress.KnownDNSProvider(a.DNSProvider) {
		return fmt.Errorf("gateway.acme.dns_provider %q is unknown (known: %v)", a.DNSProvider, ingress.KnownDNSProviders())
	}
	if c.GatewayEmail() == "" && os.Getenv("SKLP_GATEWAY_ACME_EMAIL") == "" {
		return fmt.Errorf("gateway.acme is enabled but no contact email: set gateway.email or SKLP_GATEWAY_ACME_EMAIL (Let's Encrypt rejects an empty/placeholder contact)")
	}
	return nil
}

// validateRegistries enforces that each `registries:` entry carries both a
// non-empty host key and both credential fields. Because the password is
// resolved from ${ENV_VAR} at load time (expandVars), an empty password here
// means the env var was set but blank — a misconfiguration worth surfacing
// early rather than failing with an opaque pull error at deploy time.
func (c *DeployConfig) validateRegistries() error {
	for host, spec := range c.Registries {
		if host == "" {
			return fmt.Errorf("registries: a registry host key must not be empty")
		}
		if spec.Username == "" {
			return fmt.Errorf("registries %q: username is required", host)
		}
		if spec.Password == "" {
			return fmt.Errorf("registries %q: password is required (resolved from the environment via ${VAR})", host)
		}
	}
	return nil
}

// validateBackup enforces the backup block invariants: if present, BOTH a
// command and a valid cron schedule are required. A backup without a schedule
// would never fire; a schedule without a command has nothing to run.
func validateVolumes(kind, name string, volumes []string) error {
	for _, v := range volumes {
		if err := space.ValidateNamedVolume(v); err != nil {
			return fmt.Errorf("%s %q: %w", kind, name, err)
		}
	}
	return nil
}

func validateBackup(kind, name string, b *BackupSpec) error {
	if b == nil {
		return nil
	}
	if len(b.Command) == 0 {
		return fmt.Errorf("%s %q: backup requires a command", kind, name)
	}
	if b.Schedule == "" {
		return fmt.Errorf("%s %q: backup requires a schedule", kind, name)
	}
	if _, err := ParseCron(b.Schedule); err != nil {
		return fmt.Errorf("%s %q: backup %w", kind, name, err)
	}
	return nil
}

// validateIngress enforces invariant I6 for the per-space `ingress` map: each
// target service exists, the "host:container" value parses, the container port
// matches the service's `port`, no host port is reused across two named spaces
// (a silent dead bind otherwise), and the ephemeral mould carries no fixed
// ingress (it can't be materialised twice without colliding — route by URL).
func (c *DeployConfig) validateIngress() error {
	hostOwner := map[int]string{} // host port -> "space.service" that claimed it
	for _, space := range c.sortedSpaceNames() {
		spec := c.Spaces[space]
		if len(spec.Ingress) == 0 {
			continue
		}
		if space == ephemeralKey {
			return fmt.Errorf("space %q: ingress with a fixed host port cannot be used on the ephemeral mould (it would collide across materialised spaces); route ephemeral spaces by url instead", space)
		}
		for _, svcName := range sortedKeys(spec.Ingress) {
			svc, ok := c.Services[svcName]
			if !ok {
				return fmt.Errorf("space %q: ingress targets unknown service %q", space, svcName)
			}
			m, err := parsePortMapping(spec.Ingress[svcName])
			if err != nil {
				return fmt.Errorf("space %q: ingress for service %q: %w", space, svcName, err)
			}
			if svc.Port != m.Container {
				return fmt.Errorf("space %q: ingress for service %q container port %d does not match the service port %d", space, svcName, m.Container, svc.Port)
			}
			if prev, dup := hostOwner[m.Host]; dup {
				return fmt.Errorf("ingress host port %d is claimed by both %s and %s.%s; a host port is a HOST resource and cannot be reused across spaces", m.Host, prev, space, svcName)
			}
			hostOwner[m.Host] = space + "." + svcName
		}
	}
	return nil
}

// fqdnLabel matches one DNS label: [a-z0-9] with optional internal hyphens,
// no leading/trailing hyphen, 1..63 chars (the per-label cap enforced below).
var fqdnLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// validFQDN reports whether host is a syntactically valid hostname: non-empty,
// dot-separated labels of [a-z0-9-] with no leading/trailing dot or hyphen,
// each label <=63 chars, total <=253. A single leading "*." wildcard label is
// allowed (the ephemeral/url case), e.g. "*.acme.app".
// routePathRE is the RFC 3986 path character set: the path is written into a
// traefik rule between backticks inside a double-quoted YAML string, so a
// backtick, quote, backslash or whitespace would escape both.
var routePathRE = regexp.MustCompile(`^/[A-Za-z0-9._~!$&'()*+,;=:@%/-]*$`)

func validRoutePath(path string) bool {
	return path == "" || routePathRE.MatchString(path)
}

// ValidateResolvedRoute applies the deploy.yaml route checks to a route that
// reached the node already resolved, bypassing the YAML validation.
func ValidateResolvedRoute(r ResolvedRoute) error {
	if !validFQDN(r.Host) {
		return fmt.Errorf("gateway route for service %q has invalid host %q", r.Service, r.Host)
	}
	if !validRoutePath(r.Path) {
		return fmt.Errorf("gateway route for service %q has invalid path %q", r.Service, r.Path)
	}
	return nil
}

func validFQDN(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return false
	}
	for i, label := range labels {
		if i == 0 && label == "*" {
			continue // leading wildcard label
		}
		if len(label) == 0 || len(label) > 63 || !fqdnLabel.MatchString(label) {
			return false
		}
	}
	return true
}

// validateGateway enforces the per-space `gateway.routes` list (G0): each route
// targets a long-lived service (not a job) with a non-empty `port`, the host is
// a valid FQDN, the pair (host, path) is unique across all named spaces (the
// same host with distinct paths routes to distinct services), a service is not
// in both `ingress` and a gateway route of one space (mutually exclusive: raw
// host:port vs hostname+TLS), and the ephemeral mould carries no fixed route (a
// concrete hostname can't be materialised per-PR — route ephemeral by url).
func (c *DeployConfig) validateGateway() error {
	routeOwner := map[string]string{} // "host\x00path" -> "space.service" that claimed it
	wholeHost := map[string]string{}  // host -> the space it is handed whole to
	detailed := map[string]bool{}     // host -> routed path by path
	for _, space := range c.sortedSpaceNames() {
		spec := c.Spaces[space]
		if spec.Gateway == nil || len(spec.Gateway.Routes) == 0 {
			continue
		}
		if space == ephemeralKey {
			return fmt.Errorf("space %q: a fixed gateway route cannot be used on the ephemeral mould (a concrete hostname can't be materialised per space); route ephemeral spaces by url instead", space)
		}
		for _, r := range spec.Gateway.Routes {
			whole := r.Space != ""
			if whole && r.Service != "" {
				return fmt.Errorf("space %q: gateway route for host %q sets both space and service; a route hands a host whole to a space OR routes a path to a service", space, r.Host)
			}
			if !whole && r.Service == "" {
				return fmt.Errorf("space %q: gateway route requires a service", space)
			}
			if r.Passthrough != 0 {
				if err := validatePassthroughRoute(r); err != nil {
					return fmt.Errorf("space %q: %w", space, err)
				}
			}
			if whole {
				if strings.Contains(r.Space, "/") {
					return fmt.Errorf("space %q: gateway route space %q is malformed (a bare space name)", space, r.Space)
				}
				if err := validateWholeHostRoute(r); err != nil {
					return fmt.Errorf("space %q: %w", space, err)
				}
			} else if remoteSpace, remoteSvc, mesh := splitMeshTarget(r.Service); mesh {
				// A mesh route targets a service on another node; there is
				// nothing local to validate against — the peer cache resolves
				// (or fails to resolve) it live. Only the NAME shape is checked.
				if remoteSpace == "" || remoteSvc == "" || strings.Contains(remoteSvc, "/") {
					return fmt.Errorf("space %q: gateway route target %q is malformed (mesh targets are \"<space>/<service>\")", space, r.Service)
				}
			} else {
				if _, both := spec.Ingress[r.Service]; both {
					return fmt.Errorf("space %q: service %q is in both ingress and a gateway route; they are mutually exclusive (ingress = raw host:port no-TLS, gateway = hostname + TLS)", space, r.Service)
				}
				svc, ok := c.Services[r.Service]
				if !ok {
					return fmt.Errorf("space %q: gateway route targets unknown service %q (routes target long-lived services, not jobs)", space, r.Service)
				}
				if svc.Port == 0 {
					return fmt.Errorf("space %q: gateway route for service %q requires the service to declare a port (the gateway needs a port to target)", space, r.Service)
				}
			}
			if !validFQDN(r.Host) {
				return fmt.Errorf("space %q: gateway route for service %q has invalid host %q", space, r.Service, r.Host)
			}
			if !validRoutePath(r.Path) {
				return fmt.Errorf("space %q: gateway route for service %q has invalid path %q (must start with '/' and use URL path characters only)", space, r.Service, r.Path)
			}
			if err := validateRouteKind(r.Kind); err != nil {
				return fmt.Errorf("space %q: gateway route for service %q: %w", space, r.Service, err)
			}
			if r.Rate < 0 || r.Burst < 0 {
				return fmt.Errorf("space %q: gateway route for service %q has negative rate/burst", space, r.Service)
			}
			if r.Rate > 0 && r.Burst == 0 {
				return fmt.Errorf("space %q: gateway route for service %q sets rate but burst 0 (a zero-burst bucket rejects every request); set burst >= 1", space, r.Service)
			}
			if _, err := parseRouteTimeout(r.Timeout); err != nil {
				return fmt.Errorf("space %q: gateway route for service %q: %w", space, r.Service, err)
			}
			if _, err := parseRouteMaxBody(r.MaxBody); err != nil {
				return fmt.Errorf("space %q: gateway route for service %q: %w", space, r.Service, err)
			}
			if whole {
				if prev, dup := wholeHost[r.Host]; dup {
					return fmt.Errorf("host %q is handed whole to both spaces %s and %s; a host belongs to exactly one space", r.Host, prev, r.Space)
				}
				if detailed[r.Host] {
					return fmt.Errorf("host %q is handed whole to space %s but also routed path by path; a host is routed either by its space or by this manifest, not both", r.Host, r.Space)
				}
				wholeHost[r.Host] = r.Space
				continue
			}
			if owner, dup := wholeHost[r.Host]; dup {
				return fmt.Errorf("host %q is handed whole to space %s but also routed path by path by %s.%s; a host is routed either by its space or by this manifest, not both", r.Host, owner, space, r.Service)
			}
			detailed[r.Host] = true
			key := r.Host + "\x00" + r.Path
			if prev, dup := routeOwner[key]; dup {
				where := r.Host
				if r.Path != "" {
					where += r.Path
				}
				return fmt.Errorf("route %q is claimed by both %s and %s.%s; a host+path routes to exactly one space/service", where, prev, space, r.Service)
			}
			routeOwner[key] = space + "." + r.Service
		}
	}
	return nil
}

// validateWholeHostRoute checks a `space:` route: the whole host goes to that
// space's gateway, which routes by path from the space's own manifest, so
// nothing path-level may be declared here (ADR-021).
func validateWholeHostRoute(r RouteSpec) error {
	set := []string{}
	if r.Path != "" {
		set = append(set, "path")
	}
	if r.Kind != "" {
		set = append(set, "kind")
	}
	if r.Rate != 0 || r.Burst != 0 {
		set = append(set, "rate/burst")
	}
	if r.Timeout != "" {
		set = append(set, "timeout")
	}
	if r.MaxBody != "" {
		set = append(set, "max_body")
	}
	if r.SecurityHeaders {
		set = append(set, "security_headers")
	}
	if r.Compress {
		set = append(set, "compress")
	}
	if len(set) > 0 {
		return fmt.Errorf("gateway route for host %q hands it whole to space %q, so it cannot set %s; declare them in that space's own deploy.yaml, or target service \"%s/<service>\" to route this path here", r.Host, r.Space, strings.Join(set, ", "), r.Space)
	}
	return nil
}

// nodePorts are bound by the node itself on its controller or mesh address:
// SSH, the controller's DNS and HTTP proxy, the control API, the front doors.
var nodePorts = map[int]bool{22: true, 53: true, 80: true, 6464: true, 9080: true, 9443: true}

func validatePassthroughRoute(r RouteSpec) error {
	if r.Passthrough < 1 || r.Passthrough > 65535 {
		return fmt.Errorf("gateway route for host %q: passthrough port %d out of range", r.Host, r.Passthrough)
	}
	if nodePorts[r.Passthrough] {
		return fmt.Errorf("gateway route for host %q: passthrough port %d is reserved by the node", r.Host, r.Passthrough)
	}
	if _, _, mesh := splitMeshTarget(r.Service); mesh {
		return fmt.Errorf("gateway route for host %q: passthrough cannot target a remote service; hand the host to its space with space: and passthrough", r.Host)
	}
	set := []string{}
	if r.Path != "" {
		set = append(set, "path")
	}
	if r.Kind != "" {
		set = append(set, "kind")
	}
	if r.Rate != 0 || r.Burst != 0 {
		set = append(set, "rate/burst")
	}
	if r.Timeout != "" {
		set = append(set, "timeout")
	}
	if r.MaxBody != "" {
		set = append(set, "max_body")
	}
	if r.SecurityHeaders {
		set = append(set, "security_headers")
	}
	if r.Compress {
		set = append(set, "compress")
	}
	if r.RedirectApex {
		set = append(set, "redirect_apex")
	}
	if len(set) > 0 {
		return fmt.Errorf("gateway route for host %q is a TLS passthrough, so it cannot set %s: sklp never sees its HTTP requests", r.Host, strings.Join(set, ", "))
	}
	return nil
}

// sortedSpaceNames returns the space names in deterministic order so ingress
// validation and any reported conflict are stable across runs.
func (c *DeployConfig) sortedSpaceNames() []string {
	names := make([]string, 0, len(c.Spaces))
	for n := range c.Spaces {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// NamedSpaces returns the declared, materialisable space names in deterministic
// order — every `spaces:` entry EXCEPT the ephemeral mould (which is a template,
// not a space). `sklp deploy --all` iterates this to bring up every space at
// once. A config with no `spaces:` block has no named spaces (the single base
// space is reached by any name), so this returns nil there.
func (c *DeployConfig) NamedSpaces() []string {
	out := make([]string, 0, len(c.Spaces))
	for _, n := range c.sortedSpaceNames() {
		if n == ephemeralKey {
			continue
		}
		out = append(out, n)
	}
	return out
}

// sortedKeys returns a map's keys sorted, for deterministic iteration.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// validateDependsOn checks every depends_on target of one node resolves to a
// known service OR job in THIS SAME Space's deploy (the union): an upstream
// may be either family. kind/name only colour the error message. depends_on
// is same-Space-only (ordering/wait-for-ready) — cross-Space reachability is
// resolved entirely via DNS (<service>.<space>.internal) once the target
// declares port:, never via depends_on, so a "/"-containing entry is a
// configuration error rather than a different (cross-Space) address form.
func (c *DeployConfig) validateDependsOn(kind, name string, deps []string) error {
	for _, dep := range deps {
		if strings.Contains(dep, "/") {
			return fmt.Errorf("%s %q: depends_on %q looks like a cross-Space \"space/service\" reference, "+
				"which is no longer supported — cross-Space services are reached by DNS name "+
				"(<service>.<space>.internal) once the target declares port:, not via depends_on", kind, name, dep)
		}
		_, isSvc := c.Services[dep]
		_, isJob := c.Jobs[dep]
		if !isSvc && !isJob {
			return fmt.Errorf("%s %q: depends_on references unknown service or job %q", kind, name, dep)
		}
	}
	return nil
}

// dependsOn returns the depends_on of a node in the services+jobs union, or nil
// when the name is unknown. Used by findCycle to walk the union graph.
func (c *DeployConfig) dependsOn(name string) []string {
	if s, ok := c.Services[name]; ok {
		return s.DependsOn
	}
	if j, ok := c.Jobs[name]; ok {
		return j.DependsOn
	}
	return nil
}

// findCycle returns a human-readable cycle path through depends_on if one
// exists, else "". Plain DFS with a recursion stack over the services+jobs
// union, so a cycle crossing the two families (service->job->service) is caught.
func (c *DeployConfig) findCycle() string {
	const (
		white = 0 // unvisited
		grey  = 1 // on the current DFS stack
		black = 2 // fully explored
	)
	color := make(map[string]int, len(c.Services)+len(c.Jobs))
	var stack []string
	var dfs func(name string) string
	dfs = func(name string) string {
		color[name] = grey
		stack = append(stack, name)
		for _, dep := range c.dependsOn(name) {
			switch color[dep] {
			case grey:
				return strings.Join(append(stack, dep), " -> ")
			case white:
				if cyc := dfs(dep); cyc != "" {
					return cyc
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[name] = black
		return ""
	}
	// Deterministic iteration order over the services+jobs union so the
	// reported cycle is stable and a cross-family cycle is caught.
	names := make([]string, 0, len(c.Services)+len(c.Jobs))
	for n := range c.Services {
		names = append(names, n)
	}
	for n := range c.Jobs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if color[n] == white {
			if cyc := dfs(n); cyc != "" {
				return cyc
			}
		}
	}
	return ""
}

// envVarRefs captures, from the RAW (un-expanded) YAML, every service/job/space
// env entry whose value references a ${VAR}, whole ("${DB_URL}") or composed
// ("postgres://app:${PW}@db/app"). The result maps the env KEY to that raw
// template, so the resolved value is never persisted in cleartext: the manifest
// keeps the template and ExpandTemplate rebuilds the value at launch from the
// space's secrets — the same late binding registryPasswordEnvs applies to
// registry passwords. A value whose only reference is ${SPACE} is not a secret
// and stays a literal.
func envVarRefs(raw []byte) (map[string]string, error) {
	var probe struct {
		Services map[string]struct {
			Env map[string]string `yaml:"env"`
		} `yaml:"services"`
		Jobs map[string]struct {
			Env map[string]string `yaml:"env"`
		} `yaml:"jobs"`
		// A space entry mixes reserved mould keys with per-service overrides
		// under the same mapping. The reserved ones are scalars or maps of a
		// different shape (replicas: 1, ingress: {...}), so the value is kept
		// as a raw node and decoded per key, skipping the reserved ones.
		Spaces map[string]map[string]yaml.Node `yaml:"spaces"`
	}
	if err := yaml.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("parse deploy file (env refs): %w", err)
	}
	// Gather every (service, key) -> template first, then decide. Two
	// services declaring the SAME key with DIFFERENT templates used to
	// overwrite each other here, and since Go iterates a map in random order
	// the winner changed between applies. gate lost two hours to it:
	// four DSN declarations collapsed into one, and kratos-migrate ran
	// Kratos' migrations against Hydra's database.
	//
	// Deciding in a second pass is what makes the result independent of that
	// iteration order: writing as we went would leave whichever service came
	// first holding the bare key, so the manifest's shape — not just which
	// value won — differed from one apply to the next.
	type decl struct {
		service  string
		template string
	}
	byKey := map[string][]decl{}
	gather := func(service string, env map[string]string) {
		for key, val := range env {
			if len(TemplateVarNames(val)) == 0 {
				continue
			}
			byKey[key] = append(byKey[key], decl{service: service, template: val})
		}
	}
	for name, s := range probe.Services {
		gather(name, s.Env)
	}
	for name, j := range probe.Jobs {
		gather(name, j.Env)
	}
	for _, sp := range probe.Spaces {
		for key, node := range sp {
			if _, reserved := spaceMouldKeys[key]; reserved {
				continue
			}
			var ov struct {
				Env map[string]string `yaml:"env"`
			}
			if err := node.Decode(&ov); err != nil {
				continue
			}
			gather(key, ov.Env)
		}
	}

	// A key every declaration writes identically keeps its bare name, so the
	// on-disk shape is unchanged for every manifest that never had a
	// conflict — and all of those are already deployed. A contested key is
	// scoped for EVERY declaring service, never just the losers: leaving one
	// of them on the bare key is what made the output order-dependent.
	out := map[string]string{}
	for key, decls := range byKey {
		contested := false
		for _, d := range decls[1:] {
			if d.template != decls[0].template {
				contested = true
				break
			}
		}
		if !contested {
			out[key] = decls[0].template
			continue
		}
		for _, d := range decls {
			out[ScopedEnvKey(d.service, key)] = d.template
		}
	}
	return out, nil
}

// sensitiveEnvHints are the substrings that mark an env KEY as likely secret.
// Used only to warn: the decision to inline a value is the operator's.
var sensitiveEnvHints = []string{"SECRET", "TOKEN", "PASSWORD", "PASSWD", "APIKEY", "API_KEY", "PRIVATE_KEY", "CREDENTIAL"}

// SensitiveEnvKey reports whether an env KEY looks like it holds a secret. It
// is the single definition of that heuristic: LiteralSecretKeys warns the
// operator with it at deploy time, and the control API's manifest read path
// labels an inlined value with it so a UI knows to mask it.
func SensitiveEnvKey(key string) bool {
	upper := strings.ToUpper(key)
	for _, hint := range sensitiveEnvHints {
		if strings.Contains(upper, hint) {
			return true
		}
	}
	return false
}

// LiteralSecretKeys returns the env keys that look sensitive but were written as
// a literal rather than ${VAR}. Such a value cannot be persisted by name, so it
// lands in the on-disk manifest in cleartext — the caller warns the operator.
// Sorted for a deterministic message.
func (c *DeployConfig) LiteralSecretKeys() []string {
	var out []string
	seen := map[string]struct{}{}
	scan := func(env map[string]string) {
		for key, val := range env {
			if val == "" {
				continue
			}
			if _, isRef := c.envVarRef[key]; isRef {
				continue
			}
			if _, dup := seen[key]; dup {
				continue
			}
			if SensitiveEnvKey(key) {
				seen[key] = struct{}{}
				out = append(out, key)
			}
		}
	}
	for _, s := range c.Services {
		scan(s.Env)
	}
	for _, j := range c.Jobs {
		scan(j.Env)
	}
	for _, sp := range c.Spaces {
		for _, ov := range sp.Overrides {
			scan(ov.Env)
		}
	}
	sort.Strings(out)
	return out
}
