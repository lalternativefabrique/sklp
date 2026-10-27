// Package config manages the on-disk sklp CLI configuration: a YAML file
// holding two independent things — the Skalpai SaaS endpoint ("api": one
// url, one token, no selector) and the user's deploy nodes ("contexts":
// named, selectable via "current_context").
//
// The split is the point. Both used to live in one "profiles" map behind a
// single "current" key, so picking a deploy node broke `sklp issue`, and
// pointing SKLP_PROFILE at the API profile silently retargeted deploys at
// the local socket. Legacy files still load and are rewritten on Save.
//
// File location follows the XDG Base Directory spec:
//
//	$XDG_CONFIG_HOME/sklp/config.yaml         (if XDG_CONFIG_HOME is set)
//	$HOME/.config/sklp/config.yaml            (XDG default)
//	$HOME/.sklp/config.yaml                   (fallback if .config is unavailable)
//
// Saves are atomic (tmp + rename) and the file is chmod'd to 0600. Load
// emits a stderr warning when an existing file has looser permissions.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"

	"gopkg.in/yaml.v3"
)

// EnvContext overrides the active deploy context without modifying the
// on-disk config. Replaces SKLP_PROFILE, which was ambiguous: it moved the
// API selection and the deploy target at once.
const EnvContext = "SKLP_CONTEXT"

// EnvToken overrides the SaaS token at read time (CI use case).
const EnvToken = "SKLP_TOKEN"

// EnvContextToken overrides the active deploy context's bearer. Separate
// from EnvToken on purpose — see EffectiveContextToken.
const EnvContextToken = "SKLP_CONTEXT_TOKEN"

// DefaultAPIURL is the hosted API pre-filled in `sklp auth login`. Nearly
// every login targets it, and a local instance is the case worth typing out.
const DefaultAPIURL = "https://api.skalpai.dev"

// Config is the on-disk shape persisted under
// $XDG_CONFIG_HOME/sklp/config.yaml.
//
// It holds two unrelated control planes, deliberately asymmetric:
//
//   - API: the Skalpai SaaS. Singular — one url, one account, nothing to
//     select between. Consumed by `sklp issue` / `sklp auth`.
//   - Contexts: the user's own deploy nodes. Plural and selectable via
//     CurrentContext. Consumed by `sklp deploy`.
//
// They used to share a single "current" selector, which meant
// `sklp deploy context use <node>` silently broke `sklp issue` (the node
// profile has no api_url), and conversely SKLP_PROFILE aimed at the API
// profile silently retargeted deploys at the LOCAL socket. Modelling the
// SaaS as one of several selectable profiles was the root mistake: only
// nodes are plural.
//
// Legacy "current" + "profiles" files still load (see migrate) and are
// rewritten into this shape on the next Save.
type Config struct {
	API            API                `yaml:"api,omitempty"`
	Secrets        Secrets            `yaml:"secrets,omitempty"`
	CurrentContext string             `yaml:"current_context,omitempty"`
	Contexts       map[string]Context `yaml:"contexts,omitempty"`

	// Legacy fields, read-only. Populated only when parsing an old file;
	// migrate() folds them into API/Contexts and clears them so they are
	// never written back.
	LegacyCurrent  string                   `yaml:"current,omitempty"`
	LegacyProfiles map[string]legacyProfile `yaml:"profiles,omitempty"`
}

// API is the Skalpai SaaS endpoint plus its Personal Access Token. There is
// exactly one: no name, no selector.
type API struct {
	URL   string `yaml:"url,omitempty"`
	Token string `yaml:"token,omitempty"`
}

// Secrets configures how the stack-config passphrase is obtained when
// SKLP_SECRETS_PASSPHRASE is unset.
//
// PassphraseCommand is a lookup command whose stdout is the passphrase — the
// shape git (credential.helper) and docker (credsStore) use. sklp implements
// no vault of its own: a hard-coded store would need a code change for every
// vault a user moves to (Keychain, libsecret, Proton Pass, `scw secret`),
// while a command covers all of them and any future one.
//
// It holds no secret itself, so it stays in this file rather than the vault
// it points at.
type Secrets struct {
	PassphraseCommand string `yaml:"passphrase_command,omitempty"`
}

// Context is one deploy target (kubeconfig-style): a NODE, reached over TLS,
// plus the space to assume on it when a command names none. A context with no
// Server pilots the LOCAL socket — which callers must not fall back to
// implicitly, see Config.ResolveContext.
type Context struct {
	Server        string `yaml:"server,omitempty"`
	CAFingerprint string `yaml:"ca_fingerprint,omitempty"`
	// Space is the DEFAULT space for commands against this node, never a limit
	// on which of its spaces are reachable: a node dispatches every space it
	// hosts under control.SpaceRoutePrefix. Reading it as a scope is what made
	// the cockpit refuse a node's other spaces (fixed 2026-08-31).
	Space string `yaml:"space,omitempty"`
	Token string `yaml:"token,omitempty"`
}

// legacyProfile is the pre-split on-disk shape: one bag mixing SaaS auth
// (api_url + token) and deploy transport (server + ca_fingerprint + space).
type legacyProfile struct {
	APIURL        string `yaml:"api_url"`
	Token         string `yaml:"token,omitempty"`
	Server        string `yaml:"server,omitempty"`
	CAFingerprint string `yaml:"ca_fingerprint,omitempty"`
	Space         string `yaml:"space,omitempty"`
}

// Path returns the resolved config file path. It does NOT create the
// file — only the parent directory if needed. Returns the path that
// Load/Save should use.
func Path() (string, error) {
	// SKLP_CONFIG points directly at a config file — one file per node/VM, no
	// XDG dance. Highest priority; its parent dir is created if needed.
	if p := os.Getenv("SKLP_CONFIG"); p != "" {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return "", fmt.Errorf("create config dir for SKLP_CONFIG %s: %w", p, err)
		}
		return p, nil
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		dir := filepath.Join(xdg, "sklp")
		if err := os.MkdirAll(dir, 0o700); err == nil {
			return filepath.Join(dir, "config.yaml"), nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	xdgDefault := filepath.Join(home, ".config", "sklp")
	if err := os.MkdirAll(xdgDefault, 0o700); err == nil {
		return filepath.Join(xdgDefault, "config.yaml"), nil
	}
	// Fallback: ~/.sklp — only if the XDG default cannot be created.
	fallback := filepath.Join(home, ".sklp")
	if err := os.MkdirAll(fallback, 0o700); err != nil {
		return "", fmt.Errorf("create fallback config dir %s: %w", fallback, err)
	}
	return filepath.Join(fallback, "config.yaml"), nil
}

// Load reads the config file. If missing, returns a zero Config + the
// path it would be written to (not an error). Parse errors propagate.
//
// On Unix, if the file's permissions are looser than 0600 a single
// warning line is emitted to stderr and loading continues.
func Load() (*Config, string, error) {
	path, err := Path()
	if err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Config{Contexts: map[string]Context{}}, path, nil
	}
	if err != nil {
		return nil, path, fmt.Errorf("read %s: %w", path, err)
	}

	// Permission warning (Unix only — Windows perm semantics differ).
	if runtime.GOOS != "windows" {
		if info, statErr := os.Stat(path); statErr == nil {
			mode := info.Mode().Perm()
			if mode&0o077 != 0 {
				fmt.Fprintf(os.Stderr,
					"warning: %s has loose permissions (mode %#o); recommended 0600\n",
					path, mode)
			}
		}
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, path, fmt.Errorf("parse %s: %w", path, err)
	}
	cfg.migrate()
	if cfg.Contexts == nil {
		cfg.Contexts = map[string]Context{}
	}
	return &cfg, path, nil
}

// migrate folds a legacy "current" + "profiles" file into the API/Contexts
// split. Each legacy profile can contribute to both sides:
//
//   - api_url + token  → API (first non-empty wins; a second, differing
//     api_url is reported on stderr rather than silently dropped)
//   - server/space     → a Context under the same name
//
// A legacy profile carrying only api_url yields no context, so it can never
// again be selected as a deploy target. The old "current" only carries over
// when it names a profile that actually had deploy fields.
//
// No-op on a file already in the new shape. The result is written back on
// the next Save; reads keep working either way.
func (c *Config) migrate() {
	if len(c.LegacyProfiles) == 0 {
		c.LegacyCurrent = ""
		return
	}
	if c.Contexts == nil {
		c.Contexts = map[string]Context{}
	}
	// Deterministic order: map iteration would make "first wins" a coin flip.
	names := make([]string, 0, len(c.LegacyProfiles))
	for n := range c.LegacyProfiles {
		names = append(names, n)
	}
	sort.Strings(names)

	apiFrom := ""
	for _, n := range names {
		p := c.LegacyProfiles[n]
		if p.APIURL != "" {
			switch {
			case c.API.URL == "":
				c.API = API{URL: p.APIURL, Token: p.Token}
				apiFrom = n
			case c.API.URL != p.APIURL:
				fmt.Fprintf(os.Stderr,
					"warning: config migration kept api url %q (from profile %q) and ignored %q (from profile %q)\n",
					c.API.URL, apiFrom, p.APIURL, n)
			}
		}
		// Only profiles with a real deploy transport become contexts. A
		// server-less profile pointed at the local socket implicitly; that
		// fallback is exactly what we are removing, so it is not carried over.
		if p.Server == "" {
			continue
		}
		if _, exists := c.Contexts[n]; !exists {
			c.Contexts[n] = Context{
				Server:        p.Server,
				CAFingerprint: p.CAFingerprint,
				Space:         p.Space,
				Token:         p.Token,
			}
		}
	}
	if c.CurrentContext == "" {
		if _, ok := c.Contexts[c.LegacyCurrent]; ok {
			c.CurrentContext = c.LegacyCurrent
		}
	}
	c.LegacyProfiles = nil
	c.LegacyCurrent = ""
}

// Save writes the config to path atomically: marshal → write tmp →
// chmod 0600 → rename. Creates the parent directory if needed.
func (c *Config) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create dir: %w", err)
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config.yaml.tmp-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	// Best-effort cleanup if we fail before rename.
	cleanup := func() { _ = os.Remove(tmpPath) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		cleanup()
		return fmt.Errorf("chmod temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("rename: %w", err)
	}
	// Re-apply 0600 in case the umask interfered after rename.
	_ = os.Chmod(path, 0o600)
	return nil
}

// ResolveAPI returns the SaaS endpoint. There is no selector and no
// fallback: either the file has an api block or the user has not logged in.
// Deploy state cannot influence the result.
func (c *Config) ResolveAPI() (API, error) {
	if c.API.URL == "" {
		return API{}, errors.New("no Skalpai API configured (run sklp auth login)")
	}
	return c.API, nil
}

// ResolveContext returns the active deploy context and its name:
//
//  1. flagContext (--context)
//  2. SKLP_CONTEXT env
//  3. c.CurrentContext
//
// Unlike the old profile resolution there is NO implicit default: an empty
// or unknown selection is an error, never a silent fall back to the local
// socket. Callers wanting the local socket must ask for it explicitly
// (`--local`), so a mistyped or stale context can no longer send a
// production manifest to the developer's own machine.
func (c *Config) ResolveContext(flagContext string) (string, Context, error) {
	name := flagContext
	if name == "" {
		name = os.Getenv(EnvContext)
	}
	if name == "" {
		name = c.CurrentContext
	}
	if name == "" {
		return "", Context{}, errors.New(
			"no deploy context selected (run sklp deploy context use <name>, or pass --local)")
	}
	ctx, ok := c.Contexts[name]
	if !ok {
		return name, Context{}, fmt.Errorf("deploy context %q not found in config", name)
	}
	return name, ctx, nil
}

// SetAPI stores the SaaS endpoint and token.
func (c *Config) SetAPI(a API) { c.API = a }

// SetContext upserts a deploy context by name. If no context is current,
// the freshly added one becomes current.
func (c *Config) SetContext(name string, ctx Context) {
	if c.Contexts == nil {
		c.Contexts = map[string]Context{}
	}
	c.Contexts[name] = ctx
	if c.CurrentContext == "" {
		c.CurrentContext = name
	}
}

// ClearAPIToken drops the SaaS token, keeping the url (so `sklp auth logout`
// doesn't force the user to remember it). Deploy context tokens are
// untouched: logging out of the SaaS must not disarm the deploy transport.
func (c *Config) ClearAPIToken() { c.API.Token = "" }

// EffectiveAPIToken returns the SKLP_TOKEN env override if set, else the
// persisted SaaS token. Lets CI authenticate without touching disk.
func EffectiveAPIToken(a API) string {
	if env := os.Getenv(EnvToken); env != "" {
		return env
	}
	return a.Token
}

// EffectiveContextToken returns the SKLP_CONTEXT_TOKEN env override if set,
// else the context's persisted bearer.
//
// Deliberately a different env var from EffectiveAPIToken: these authorise
// very different things — a SaaS PAT reads issues, a context bearer drives
// the deploy control API of a production node. The old code funnelled both
// through SKLP_TOKEN, so exporting a CI token for `sklp issue` also silently
// re-authenticated deploys.
func EffectiveContextToken(ctx Context) string {
	if env := os.Getenv(EnvContextToken); env != "" {
		return env
	}
	return ctx.Token
}
