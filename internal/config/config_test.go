package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// withXDG points XDG_CONFIG_HOME at a temp dir and restores it on cleanup.
// Returns the path config.Path() will resolve to inside that temp.
func withXDG(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	// Make sure HOME is also pinned so a fallback can't escape the temp.
	t.Setenv("HOME", dir)
	// Neutralise inherited overrides so resolution is decided by the file.
	t.Setenv(EnvContext, "")
	t.Setenv(EnvToken, "")
	t.Setenv(EnvContextToken, "")
	return filepath.Join(dir, "sklp", "config.yaml")
}

// writeConfigFile writes raw YAML at the resolved config path, 0600.
func writeConfigFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// captureStderr redirects os.Stderr for the duration of fn and returns what
// was written to it.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	old := os.Stderr
	os.Stderr = w
	fn()
	os.Stderr = old
	_ = w.Close()
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	_ = r.Close()
	return string(buf[:n])
}

func TestPath_UsesXDG(t *testing.T) {
	want := withXDG(t)
	got, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if got != want {
		t.Fatalf("Path = %s, want %s", got, want)
	}
}

// TestPath_SklpConfigWins: SKLP_CONFIG points Path at an exact file, beating
// XDG_CONFIG_HOME. This is what `sklp deploy ls --config shop.yaml` relies on.
func TestPath_SklpConfigWins(t *testing.T) {
	withXDG(t) // set XDG too, to prove SKLP_CONFIG takes precedence
	want := filepath.Join(t.TempDir(), "shop.yaml")
	t.Setenv("SKLP_CONFIG", want)
	got, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if got != want {
		t.Fatalf("Path = %s, want %s (SKLP_CONFIG must win)", got, want)
	}
}

func TestLoad_MissingFileReturnsZero(t *testing.T) {
	withXDG(t)
	cfg, path, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg == nil {
		t.Fatalf("cfg is nil")
	}
	if len(cfg.Contexts) != 0 {
		t.Fatalf("expected 0 contexts, got %d", len(cfg.Contexts))
	}
	if cfg.API.URL != "" || cfg.API.Token != "" {
		t.Fatalf("expected zero api block, got %+v", cfg.API)
	}
	if path == "" {
		t.Fatalf("path empty")
	}
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	withXDG(t)
	cfg := &Config{
		API:            API{URL: "https://api.skalpel.com", Token: "sklp_pat_abc_xyz"},
		CurrentContext: "prod",
		Contexts: map[string]Context{
			"prod": {Server: "https://vm:9443", CAFingerprint: "fp-prod", Space: "production", Token: "tok-prod"},
			"ci":   {Server: "https://ci:9443", Token: "tok-ci"},
		},
	}
	path, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Permission check (Unix only).
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Fatalf("mode = %#o, want 0600", mode)
		}
	}

	got, _, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.API != cfg.API {
		t.Fatalf("API = %+v, want %+v", got.API, cfg.API)
	}
	if got.CurrentContext != "prod" {
		t.Fatalf("CurrentContext = %q, want prod", got.CurrentContext)
	}
	if len(got.Contexts) != 2 {
		t.Fatalf("want 2 contexts, got %d", len(got.Contexts))
	}
	if got.Contexts["prod"] != cfg.Contexts["prod"] {
		t.Fatalf("prod context mismatch: %+v", got.Contexts["prod"])
	}
}

func TestResolveAPI(t *testing.T) {
	withXDG(t)
	empty := &Config{}
	if _, err := empty.ResolveAPI(); err == nil {
		t.Fatal("expected an error when no api block is configured")
	}
	cfg := &Config{API: API{URL: "https://api.skalpel.com", Token: "sklp_pat_x"}}
	api, err := cfg.ResolveAPI()
	if err != nil {
		t.Fatalf("ResolveAPI: %v", err)
	}
	if api.URL != "https://api.skalpel.com" || api.Token != "sklp_pat_x" {
		t.Fatalf("api = %+v", api)
	}
}

// TestResolveAPI_IgnoresContextState: the SaaS endpoint must not depend on the
// deploy selector. Switching context used to break `sklp issue`.
func TestResolveAPI_IgnoresContextState(t *testing.T) {
	withXDG(t)
	cfg := &Config{
		API:            API{URL: "https://api.skalpel.com", Token: "sklp_pat_x"},
		CurrentContext: "prod-node",
		Contexts:       map[string]Context{"prod-node": {Server: "https://vm:9443"}},
	}
	t.Setenv(EnvContext, "prod-node")
	api, err := cfg.ResolveAPI()
	if err != nil {
		t.Fatalf("ResolveAPI: %v", err)
	}
	if api.URL != "https://api.skalpel.com" {
		t.Fatalf("api url = %q, want the saas url regardless of context", api.URL)
	}
}

func TestResolveContext_PriorityOrder(t *testing.T) {
	withXDG(t)
	cfg := &Config{
		CurrentContext: "current-one",
		Contexts: map[string]Context{
			"current-one": {Server: "s2"},
			"env-one":     {Server: "s3"},
			"flag-one":    {Server: "s4"},
		},
	}

	// 1) Flag wins.
	t.Setenv(EnvContext, "env-one")
	name, c, err := cfg.ResolveContext("flag-one")
	if err != nil || name != "flag-one" || c.Server != "s4" {
		t.Fatalf("flag should win: name=%q server=%q err=%v", name, c.Server, err)
	}
	// 2) Env wins when no flag.
	name, c, err = cfg.ResolveContext("")
	if err != nil || name != "env-one" || c.Server != "s3" {
		t.Fatalf("env should win: name=%q server=%q err=%v", name, c.Server, err)
	}
	// 3) CurrentContext wins when no flag and no env.
	t.Setenv(EnvContext, "")
	name, c, err = cfg.ResolveContext("")
	if err != nil || name != "current-one" || c.Server != "s2" {
		t.Fatalf("current should win: name=%q server=%q err=%v", name, c.Server, err)
	}
}

// TestResolveContext_NothingSelectedErrors locks the security fix: with no
// flag, no env and no current_context there is NO implicit default. The old
// code fell back to a "default" profile — and a profile with no server pilots
// the LOCAL socket, so a stale selection silently deployed a production
// manifest onto the developer's own machine.
func TestResolveContext_NothingSelectedErrors(t *testing.T) {
	withXDG(t)
	cases := []struct {
		name string
		cfg  *Config
	}{
		{"no contexts at all", &Config{}},
		{"empty context map", &Config{Contexts: map[string]Context{}}},
		{
			// A "default" context EXISTS but is not current: it must still not
			// be picked up implicitly.
			name: "default context exists but is not current",
			cfg: &Config{Contexts: map[string]Context{
				"default": {Server: "https://vm:9443"},
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, c, err := tc.cfg.ResolveContext("")
			if err == nil {
				t.Fatalf("expected an error, got name=%q ctx=%+v", name, c)
			}
			if c != (Context{}) {
				t.Fatalf("expected a zero context alongside the error, got %+v", c)
			}
			if !strings.Contains(err.Error(), "no deploy context selected") {
				t.Fatalf("error should name the missing selection, got %q", err)
			}
		})
	}
}

func TestResolveContext_UnknownNameErrors(t *testing.T) {
	withXDG(t)
	cfg := &Config{Contexts: map[string]Context{"prod": {Server: "s"}}}
	name, _, err := cfg.ResolveContext("nope")
	if err == nil {
		t.Fatal("expected error for unknown context")
	}
	if name != "nope" {
		t.Fatalf("name = %q, want the requested name echoed back", name)
	}
}

func TestSetAPI(t *testing.T) {
	cfg := &Config{}
	cfg.SetAPI(API{URL: "u", Token: "t"})
	if cfg.API.URL != "u" || cfg.API.Token != "t" {
		t.Fatalf("API = %+v", cfg.API)
	}
	// Setting the api block must not invent a deploy context.
	if len(cfg.Contexts) != 0 {
		t.Fatalf("SetAPI created contexts: %+v", cfg.Contexts)
	}
}

func TestSetContext_SetsCurrentWhenEmpty(t *testing.T) {
	cfg := &Config{}
	cfg.SetContext("foo", Context{Server: "s1"})
	if cfg.CurrentContext != "foo" {
		t.Fatalf("CurrentContext = %q, want foo", cfg.CurrentContext)
	}
	cfg.SetContext("bar", Context{Server: "s2"})
	if cfg.CurrentContext != "foo" {
		t.Fatalf("CurrentContext should stay foo, got %q", cfg.CurrentContext)
	}
	if cfg.Contexts["bar"].Server != "s2" {
		t.Fatalf("bar not upserted: %+v", cfg.Contexts)
	}
}

// TestClearAPIToken_KeepsURLAndContexts: `sklp auth logout` drops the SaaS
// token only — the url stays, and deploy bearers are NOT disarmed.
func TestClearAPIToken_KeepsURLAndContexts(t *testing.T) {
	cfg := &Config{
		API:      API{URL: "u", Token: "sklp_pat_x"},
		Contexts: map[string]Context{"prod": {Server: "s", Token: "node-bearer"}},
	}
	cfg.ClearAPIToken()
	if cfg.API.Token != "" {
		t.Fatalf("api token not cleared: %q", cfg.API.Token)
	}
	if cfg.API.URL != "u" {
		t.Fatalf("api url should remain: %q", cfg.API.URL)
	}
	if cfg.Contexts["prod"].Token != "node-bearer" {
		t.Fatalf("context token must survive an api logout: %+v", cfg.Contexts["prod"])
	}
}

// TestEffectiveTokens_SeparateEnvVars: the two tokens authorise very different
// things (a SaaS PAT reads issues; a context bearer drives a node's deploy
// control API), so they read DIFFERENT env vars and must not cross-contaminate.
func TestEffectiveTokens_SeparateEnvVars(t *testing.T) {
	api := API{Token: "api-from-file"}
	ctx := Context{Token: "ctx-from-file"}

	t.Setenv(EnvToken, "")
	t.Setenv(EnvContextToken, "")
	if got := EffectiveAPIToken(api); got != "api-from-file" {
		t.Fatalf("api token = %q, want api-from-file", got)
	}
	if got := EffectiveContextToken(ctx); got != "ctx-from-file" {
		t.Fatalf("context token = %q, want ctx-from-file", got)
	}

	// SKLP_TOKEN overrides the SaaS token ONLY. Exporting a CI PAT for
	// `sklp issue` must not re-authenticate deploys.
	t.Setenv(EnvToken, "api-from-env")
	if got := EffectiveAPIToken(api); got != "api-from-env" {
		t.Fatalf("api token = %q, want api-from-env", got)
	}
	if got := EffectiveContextToken(ctx); got != "ctx-from-file" {
		t.Fatalf("SKLP_TOKEN leaked into the context token: %q", got)
	}

	// SKLP_CONTEXT_TOKEN overrides the deploy bearer ONLY.
	t.Setenv(EnvToken, "")
	t.Setenv(EnvContextToken, "ctx-from-env")
	if got := EffectiveContextToken(ctx); got != "ctx-from-env" {
		t.Fatalf("context token = %q, want ctx-from-env", got)
	}
	if got := EffectiveAPIToken(api); got != "api-from-file" {
		t.Fatalf("SKLP_CONTEXT_TOKEN leaked into the api token: %q", got)
	}

	// Both set at once: each reads its own.
	t.Setenv(EnvToken, "api-from-env")
	if got, want := EffectiveAPIToken(api), "api-from-env"; got != want {
		t.Fatalf("api token = %q, want %q", got, want)
	}
	if got, want := EffectiveContextToken(ctx), "ctx-from-env"; got != want {
		t.Fatalf("context token = %q, want %q", got, want)
	}
}

func TestLoad_LoosePermsWarn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("perm semantics differ on windows")
	}
	withXDG(t)
	path, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("api:\n  url: http://localhost:4100\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	out := captureStderr(t, func() {
		if _, _, err := Load(); err != nil {
			t.Errorf("Load: %v", err)
		}
	})
	if !strings.Contains(out, "loose permissions") {
		t.Fatalf("expected warning in stderr, got %q", out)
	}
}

// --- migrate() -------------------------------------------------------------
//
// migrate is the riskiest part of the split: it reinterprets an old
// {current, profiles} file, and a mistake there either loses the user's SaaS
// token or resurrects the server-less "deploy to my own laptop" fallback.

// TestMigrate_TypicalLegacyFile: the common shape — a "default" profile
// carrying the SaaS credentials, a node profile carrying the deploy transport,
// and `current` naming the node.
func TestMigrate_TypicalLegacyFile(t *testing.T) {
	path := withXDG(t)
	writeConfigFile(t, path, ""+
		"current: prod-node\n"+
		"profiles:\n"+
		"  default:\n"+
		"    api_url: https://api.skalpel.com\n"+
		"    token: sklp_pat_saas\n"+
		"  prod-node:\n"+
		"    server: https://vm.example:9443\n"+
		"    ca_fingerprint: fp-abc\n"+
		"    space: production\n"+
		"    token: node-bearer\n")

	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.API != (API{URL: "https://api.skalpel.com", Token: "sklp_pat_saas"}) {
		t.Fatalf("API = %+v, want the SaaS credentials from the default profile", cfg.API)
	}
	if len(cfg.Contexts) != 1 {
		t.Fatalf("want exactly 1 context (only prod-node has a server), got %+v", cfg.Contexts)
	}
	want := Context{Server: "https://vm.example:9443", CAFingerprint: "fp-abc", Space: "production", Token: "node-bearer"}
	if cfg.Contexts["prod-node"] != want {
		t.Fatalf("prod-node = %+v, want %+v", cfg.Contexts["prod-node"], want)
	}
	if cfg.CurrentContext != "prod-node" {
		t.Fatalf("CurrentContext = %q, want prod-node", cfg.CurrentContext)
	}
	// Legacy fields must be drained so they are never written back.
	if cfg.LegacyCurrent != "" || cfg.LegacyProfiles != nil {
		t.Fatalf("legacy fields not cleared: current=%q profiles=%+v", cfg.LegacyCurrent, cfg.LegacyProfiles)
	}
	// The SaaS profile is not a deploy target any more.
	if _, ok := cfg.Contexts["default"]; ok {
		t.Fatal("api-only profile became a selectable deploy context")
	}
}

// TestMigrate_APIOnlyProfileYieldsNoContext: a profile with only api_url must
// produce NO context. Otherwise it stays selectable as a deploy target, and a
// context with no server means the local socket.
func TestMigrate_APIOnlyProfileYieldsNoContext(t *testing.T) {
	path := withXDG(t)
	writeConfigFile(t, path, ""+
		"current: default\n"+
		"profiles:\n"+
		"  default:\n"+
		"    api_url: http://localhost:4100\n"+
		"    token: sklp_pat_abc\n")

	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Contexts) != 0 {
		t.Fatalf("api-only profile produced contexts: %+v", cfg.Contexts)
	}
	if cfg.API != (API{URL: "http://localhost:4100", Token: "sklp_pat_abc"}) {
		t.Fatalf("API = %+v", cfg.API)
	}
}

// TestMigrate_CurrentPointingAtServerlessProfileStaysEmpty: `current` only
// carries over when it names a profile that actually had deploy fields. No
// fallback — the user must re-select, and until then deploys refuse.
func TestMigrate_CurrentPointingAtServerlessProfileStaysEmpty(t *testing.T) {
	path := withXDG(t)
	writeConfigFile(t, path, ""+
		"current: default\n"+
		"profiles:\n"+
		"  default:\n"+
		"    api_url: http://localhost:4100\n"+
		"    token: sklp_pat_abc\n"+
		"  prod-node:\n"+
		"    server: https://vm.example:9443\n")

	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CurrentContext != "" {
		t.Fatalf("CurrentContext = %q, want empty (the old current had no server)", cfg.CurrentContext)
	}
	// And the consequence: deploys refuse rather than hitting the local socket.
	if _, _, err := cfg.ResolveContext(""); err == nil {
		t.Fatal("ResolveContext should error after migrating a server-less current")
	}
}

// TestMigrate_ConflictingAPIURLs: two profiles with different api_url — the
// first in ALPHABETICAL order wins (map iteration order would make this a coin
// flip), the loser is reported on stderr rather than silently dropped.
func TestMigrate_ConflictingAPIURLs(t *testing.T) {
	// Several profiles, deliberately written in non-alphabetical order in the
	// file, so only the explicit sort can produce a stable answer.
	body := "" +
		"profiles:\n" +
		"  zulu:\n" +
		"    api_url: https://zulu.example\n" +
		"    token: tok-zulu\n" +
		"  mike:\n" +
		"    api_url: https://mike.example\n" +
		"    token: tok-mike\n" +
		"  alpha:\n" +
		"    api_url: https://alpha.example\n" +
		"    token: tok-alpha\n" +
		"  bravo:\n" +
		"    api_url: https://bravo.example\n" +
		"    token: tok-bravo\n"

	// Repeat: Go randomises map iteration per run, so a single pass could pass
	// by luck if the sort were removed.
	for i := 0; i < 20; i++ {
		path := withXDG(t)
		writeConfigFile(t, path, body)

		var cfg *Config
		out := captureStderr(t, func() {
			var err error
			cfg, _, err = Load()
			if err != nil {
				t.Errorf("Load: %v", err)
			}
		})
		if cfg == nil {
			t.Fatal("Load returned nil config")
		}
		if cfg.API.URL != "https://alpha.example" || cfg.API.Token != "tok-alpha" {
			t.Fatalf("iteration %d: API = %+v, want the alphabetically first profile", i, cfg.API)
		}
		for _, ignored := range []string{"bravo", "mike", "zulu"} {
			if !strings.Contains(out, ignored) {
				t.Fatalf("iteration %d: stderr should report ignored profile %q, got %q", i, ignored, out)
			}
		}
		if !strings.Contains(out, "https://alpha.example") {
			t.Fatalf("iteration %d: stderr should name the kept url, got %q", i, out)
		}
		if len(cfg.Contexts) != 0 {
			t.Fatalf("iteration %d: api-only profiles produced contexts: %+v", i, cfg.Contexts)
		}
	}
}

// TestMigrate_NoopOnNewFormat: a file already in the new shape is untouched,
// and no migration warning is emitted.
func TestMigrate_NoopOnNewFormat(t *testing.T) {
	path := withXDG(t)
	writeConfigFile(t, path, ""+
		"api:\n"+
		"  url: https://api.skalpel.com\n"+
		"  token: sklp_pat_saas\n"+
		"current_context: prod\n"+
		"contexts:\n"+
		"  prod:\n"+
		"    server: https://vm:9443\n"+
		"    ca_fingerprint: fp\n"+
		"    space: production\n"+
		"    token: node-bearer\n")

	var cfg *Config
	out := captureStderr(t, func() {
		var err error
		cfg, _, err = Load()
		if err != nil {
			t.Errorf("Load: %v", err)
		}
	})
	if strings.Contains(out, "migration") {
		t.Fatalf("no migration should run on a new-format file, stderr = %q", out)
	}
	if cfg.API != (API{URL: "https://api.skalpel.com", Token: "sklp_pat_saas"}) {
		t.Fatalf("API = %+v", cfg.API)
	}
	if cfg.CurrentContext != "prod" {
		t.Fatalf("CurrentContext = %q", cfg.CurrentContext)
	}
	want := Context{Server: "https://vm:9443", CAFingerprint: "fp", Space: "production", Token: "node-bearer"}
	if cfg.Contexts["prod"] != want {
		t.Fatalf("prod = %+v, want %+v", cfg.Contexts["prod"], want)
	}
}

// TestMigrate_RoundTripRewritesFile: loading a legacy file and saving it back
// writes the NEW format — no `current:` and no `profiles:` keys survive.
func TestMigrate_RoundTripRewritesFile(t *testing.T) {
	path := withXDG(t)
	writeConfigFile(t, path, ""+
		"current: prod-node\n"+
		"profiles:\n"+
		"  default:\n"+
		"    api_url: https://api.skalpel.com\n"+
		"    token: sklp_pat_saas\n"+
		"  prod-node:\n"+
		"    server: https://vm.example:9443\n"+
		"    ca_fingerprint: fp-abc\n"+
		"    space: production\n"+
		"    token: node-bearer\n")

	cfg, gotPath, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.Save(gotPath); err != nil {
		t.Fatalf("Save: %v", err)
	}

	raw, err := os.ReadFile(gotPath)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	text := string(raw)
	for _, gone := range []string{"current:", "profiles:", "api_url:"} {
		if strings.Contains(text, gone) {
			t.Fatalf("legacy key %q still written:\n%s", gone, text)
		}
	}
	for _, want := range []string{"api:", "current_context:", "contexts:"} {
		if !strings.Contains(text, want) {
			t.Fatalf("new-format key %q missing:\n%s", want, text)
		}
	}

	// Reloading the rewritten file yields the same values.
	again, _, err := Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if again.API != cfg.API {
		t.Fatalf("API drifted: %+v vs %+v", again.API, cfg.API)
	}
	if again.CurrentContext != "prod-node" {
		t.Fatalf("CurrentContext = %q", again.CurrentContext)
	}
	if again.Contexts["prod-node"] != cfg.Contexts["prod-node"] {
		t.Fatalf("context drifted: %+v vs %+v", again.Contexts["prod-node"], cfg.Contexts["prod-node"])
	}
}
