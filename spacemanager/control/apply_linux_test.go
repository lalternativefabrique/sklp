package control

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/spacemanager"
)

// TestApplySpecRoundTrip: the opaque spec the client posts round-trips through
// MarshalApplySpec -> ApplyRequest.Spec -> applySpec without the non-gated DTO
// ever naming a deploy type.
func TestApplySpecRoundTrip(t *testing.T) {
	svcs := []spacemanager.Service{
		{Name: "api", Image: "img:1", Replicas: 2, Port: 8080},
		{Name: "web", Image: "web:1"},
	}
	meta := spacemanager.SpaceMeta{Name: "production"}
	spec, err := MarshalApplySpec(svcs, meta)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Wrap as the client does, then decode as the server does.
	body, _ := json.Marshal(ApplyRequest{Spec: spec})
	var req ApplyRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	var got applySpec
	if err := json.Unmarshal(req.Spec, &got); err != nil {
		t.Fatalf("decode spec: %v", err)
	}
	if len(got.Services) != 2 || got.Services[0].Name != "api" || got.Services[0].Replicas != 2 {
		t.Fatalf("services = %+v", got.Services)
	}
	if got.Meta.Name != "production" {
		t.Fatalf("meta = %+v", got.Meta)
	}
}

// TestApplyPushesRegistryAuths: a remote apply carrying a registry credential
// (username + a shipped Password) sets the resolved auth on the supervisor
// BEFORE services start, so a private-image pull authenticates even though the
// detached agent's own env has no such secret. This is the remote-pull fix.
func TestApplyPushesRegistryAuths(t *testing.T) {
	sup := &fakeSupervisor{}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	meta := spacemanager.SpaceMeta{
		Name: "production",
		Registries: map[string]spacemanager.RegistryAuthMeta{
			"rg.example.com": {Username: "nologin", Password: "s3cr3t-shipped"},
		},
	}
	spec, _ := MarshalApplySpec([]spacemanager.Service{{Name: "core", Image: "rg.example.com/app:1"}}, meta)
	if _, err := c.Apply(context.Background(), spec); err != nil {
		t.Fatalf("apply: %v", err)
	}
	auth, ok := sup.registryAuth["rg.example.com"]
	if !ok {
		t.Fatal("SetRegistryAuths was not called with the registry host")
	}
	if auth.Username != "nologin" || auth.Password != "s3cr3t-shipped" {
		t.Fatalf("resolved auth = %q/%q, want nologin/s3cr3t-shipped", auth.Username, auth.Password)
	}
}

// TestApplyUnchanged: applying the same manifest a live supervisor already runs
// (no service differs) reports unchanged and mutates nothing.
func TestApplyUnchanged(t *testing.T) {
	sup := &fakeSupervisor{
		live:    map[string]spacemanager.Service{"api": {Name: "api", Image: "img:1", DeclaredImage: "img:1"}},
		differs: map[string]bool{"api": false},
	}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	spec, _ := MarshalApplySpec([]spacemanager.Service{{Name: "api", Image: "img:1"}}, spacemanager.SpaceMeta{Name: "production"})
	res, err := c.Apply(context.Background(), spec)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Result != ResultUnchanged {
		t.Fatalf("result = %q, want unchanged", res.Result)
	}
	if len(sup.added)+len(sup.removed)+len(sup.reconfigured) != 0 {
		t.Fatalf("unchanged apply mutated: +%v ~%v -%v", sup.added, sup.reconfigured, sup.removed)
	}
}

// TestApplyConfigured: a manifest that adds one service, reconfigures a changed
// one, and drops a gone one reports configured with all three changes.
func TestApplyConfigured(t *testing.T) {
	sup := &fakeSupervisor{
		live: map[string]spacemanager.Service{
			"api": {Name: "api", Image: "img:1"},
			"old": {Name: "old", Image: "old:1"},
		},
		differs: map[string]bool{"api": true}, // api spec moved
	}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	spec, _ := MarshalApplySpec([]spacemanager.Service{
		{Name: "api", Image: "img:2"}, // reconfigure
		{Name: "new", Image: "new:1"}, // add
		// "old" omitted -> remove
	}, spacemanager.SpaceMeta{Name: "production"})
	res, err := c.Apply(context.Background(), spec)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Result != ResultConfigured {
		t.Fatalf("result = %q, want configured", res.Result)
	}
	if len(sup.added) != 1 || sup.added[0] != "new" {
		t.Fatalf("added = %v, want [new]", sup.added)
	}
	if len(sup.removed) != 1 || sup.removed[0] != "old" {
		t.Fatalf("removed = %v, want [old]", sup.removed)
	}
	if len(sup.reconfigured) != 1 || sup.reconfigured[0] != "api" {
		t.Fatalf("reconfigured = %v, want [api]", sup.reconfigured)
	}
	if len(res.Changes) != 3 {
		t.Fatalf("changes = %v, want 3", res.Changes)
	}
}

// TestApplyForwardsRoutes: every apply hands the manifest's Meta.Routes to the
// supervisor's SetRoutes so a re-apply that adds/changes routes converges the
// live front door (the bug: new services were added but never routed).
func TestApplyForwardsRoutes(t *testing.T) {
	sup := &fakeSupervisor{
		live:    map[string]spacemanager.Service{"api": {Name: "api", Image: "img:1"}},
		differs: map[string]bool{"api": false},
	}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	routes := []spacemanager.ResolvedRoute{
		{Service: "api", Host: "api.test"},
		{Service: "api", Host: "app.test", Path: "/api"},
	}
	spec, _ := MarshalApplySpec(
		[]spacemanager.Service{{Name: "api", Image: "img:1"}},
		spacemanager.SpaceMeta{Name: "production", Routes: routes},
	)
	if _, err := c.Apply(context.Background(), spec); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(sup.routesSet) != 2 || sup.routesSet[0].Host != "api.test" || sup.routesSet[1].Path != "/api" {
		t.Fatalf("SetRoutes got %+v, want the 2 manifest routes", sup.routesSet)
	}
}

// TestApplyRouteOnlyChange: a manifest whose SERVICES are unchanged but whose
// routes moved reports configured with an "updated gateway routes" change.
func TestApplyRouteOnlyChange(t *testing.T) {
	sup := &fakeSupervisor{
		live:          map[string]spacemanager.Service{"api": {Name: "api", Image: "img:1", DeclaredImage: "img:1"}},
		differs:       map[string]bool{"api": false},
		routesChanged: true, // supervisor reports the route set moved
	}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	spec, _ := MarshalApplySpec(
		[]spacemanager.Service{{Name: "api", Image: "img:1"}},
		spacemanager.SpaceMeta{Name: "production", Routes: []spacemanager.ResolvedRoute{{Service: "api", Host: "api.test"}}},
	)
	res, err := c.Apply(context.Background(), spec)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Result != ResultConfigured {
		t.Fatalf("result = %q, want configured", res.Result)
	}
	if len(res.Changes) != 1 || res.Changes[0] != "updated gateway routes" {
		t.Fatalf("changes = %v, want [updated gateway routes]", res.Changes)
	}
	if len(sup.added)+len(sup.removed)+len(sup.reconfigured) != 0 {
		t.Fatalf("route-only apply mutated services: +%v ~%v -%v", sup.added, sup.reconfigured, sup.removed)
	}
}

// TestApplyCreated: the first apply against a supervisor with NO desired
// services yet (empty live set) reports created, not configured.
func TestApplyCreated(t *testing.T) {
	sup := &fakeSupervisor{live: map[string]spacemanager.Service{}}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	spec, _ := MarshalApplySpec([]spacemanager.Service{
		{Name: "api", Image: "img:1"},
		{Name: "web", Image: "web:1"},
	}, spacemanager.SpaceMeta{Name: "production"})
	res, err := c.Apply(context.Background(), spec)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Result != ResultCreated {
		t.Fatalf("result = %q, want created", res.Result)
	}
	if len(sup.added) != 2 {
		t.Fatalf("added = %v, want 2 services", sup.added)
	}
}

// TestValidServiceName: the strict DNS-label whitelist accepts ordinary names
// and rejects traversal/case/empty/space/slash shapes.
func TestValidServiceName(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"web", true},
		{"db", true},
		{"api-1", true},
		{"a", true},
		{"123", true},
		{"../x", false},
		{"../../tmp/sklp-pwn", false},
		{"UPPER", false},
		{"_x", false},
		{"", false},
		{"a/b", false},
		{"a b", false},
		{"-lead", false},
		{"a.b", false},
	}
	for _, c := range cases {
		err := validServiceName(c.name)
		if c.ok && err != nil {
			t.Errorf("validServiceName(%q) = %v, want nil", c.name, err)
		}
		if !c.ok && err == nil {
			t.Errorf("validServiceName(%q) = nil, want error", c.name)
		}
	}
}

// TestApplyInvalidServiceNameRejected: an apply carrying a path-traversal name
// is rejected wholesale (error -> 400) and mutates NOTHING — no AddService.
func TestApplyInvalidServiceNameRejected(t *testing.T) {
	sup := &fakeSupervisor{live: map[string]spacemanager.Service{}}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	spec, _ := MarshalApplySpec([]spacemanager.Service{
		{Name: "web", Image: "img:1"},
		{Name: "../../../tmp/sklp-pwn", Image: "img:1"},
	}, spacemanager.SpaceMeta{Name: "production"})
	if _, err := c.Apply(context.Background(), spec); err == nil {
		t.Fatal("want error for invalid service name, got nil")
	}
	if len(sup.added) != 0 {
		t.Fatalf("invalid-name apply must not add any service, added=%v", sup.added)
	}
}

// TestApplyEmptyManifestRejected: an apply with no services is a 400, not a
// silent teardown of the whole space.
func TestApplyEmptyManifestRejected(t *testing.T) {
	sup := &fakeSupervisor{live: map[string]spacemanager.Service{"api": {Name: "api"}}}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	spec, _ := MarshalApplySpec(nil, spacemanager.SpaceMeta{Name: "production"})
	if _, err := c.Apply(context.Background(), spec); err == nil {
		t.Fatal("want error for empty manifest, got nil")
	}
	if len(sup.removed) != 0 {
		t.Fatalf("empty manifest must not remove services, removed=%v", sup.removed)
	}
}

// TestApplyPersistsManifest: a remote apply writes the desired state to disk so
// a restarted agent can adopt it. Until this was fixed only the LOCAL deploy
// path called SaveManifest, so a space deployed over --server left nothing
// behind and an agent restart came up with zero services.
func TestApplyPersistsManifest(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	sup := &fakeSupervisor{}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	meta := spacemanager.SpaceMeta{Name: "production"}
	svcs := []spacemanager.Service{{Name: "core", Image: "img:1"}}
	spec, _ := MarshalApplySpec(svcs, meta)
	if _, err := c.Apply(context.Background(), spec); err != nil {
		t.Fatalf("apply: %v", err)
	}

	got, gotMeta, err := LoadManifest("production")
	if err != nil {
		t.Fatalf("LoadManifest after apply: %v (the agent would restart empty)", err)
	}
	if gotMeta.Name != "production" {
		t.Fatalf("persisted meta.Name = %q, want production", gotMeta.Name)
	}
	if len(got) != 1 || got[0].Name != "core" || got[0].Image != "img:1" {
		t.Fatalf("persisted services = %+v, want one core/img:1", got)
	}
}

// TestApplyOverwritesClientGatewayMode: the front-door mode is a node-owned
// property. A remote apply that ships a different (or stale) spec.Meta.GatewayMode
// must not be trusted — the node's own resolved mode (Supervisor.GatewayMode)
// always wins, both on the live agent's meta and in the persisted manifest. This
// is the regression the ticket describes: whichever app deployed FIRST used to
// decide the node's permanent front door via its own deploy.yaml.
func TestApplyOverwritesClientGatewayMode(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	sup := &fakeSupervisor{gatewayMode: spacemanager.GatewayModeSingleton}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	meta := spacemanager.SpaceMeta{Name: "production", GatewayMode: spacemanager.GatewayModePerSpace}
	svcs := []spacemanager.Service{{Name: "core", Image: "img:1"}}
	spec, _ := MarshalApplySpec(svcs, meta)
	if _, err := c.Apply(context.Background(), spec); err != nil {
		t.Fatalf("apply: %v", err)
	}

	_, gotMeta, err := LoadManifest("production")
	if err != nil {
		t.Fatalf("LoadManifest after apply: %v", err)
	}
	if gotMeta.GatewayMode != spacemanager.GatewayModeSingleton {
		t.Fatalf("persisted meta.GatewayMode = %q, want the node's own %q (client-supplied %q must be ignored)",
			gotMeta.GatewayMode, spacemanager.GatewayModeSingleton, spacemanager.GatewayModePerSpace)
	}
}

// TestApplyPersistedManifestHasNoRegistryPassword: the resolved registry
// password is transport-only. It must reach the live supervisor in memory (so
// private pulls work) but never the on-disk manifest, which keeps the name-only
// shape documented on RegistryAuthMeta.
func TestApplyPersistedManifestHasNoRegistryPassword(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)

	sup := &fakeSupervisor{}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	meta := spacemanager.SpaceMeta{
		Name: "production",
		Registries: map[string]spacemanager.RegistryAuthMeta{
			"rg.example.com": {Username: "nologin", PasswordEnv: "RG_PASS", Password: "s3cr3t-shipped"},
		},
	}
	spec, _ := MarshalApplySpec([]spacemanager.Service{{Name: "core", Image: "rg.example.com/app:1"}}, meta)
	if _, err := c.Apply(context.Background(), spec); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// In memory: the launcher still got the resolved secret.
	if auth := sup.registryAuth["rg.example.com"]; auth.Password != "s3cr3t-shipped" {
		t.Fatalf("live auth password = %q, want the resolved secret", auth.Password)
	}

	// On disk: stripped, but PasswordEnv kept so the local path can re-resolve.
	_, gotMeta, err := LoadManifest("production")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	persisted := gotMeta.Registries["rg.example.com"]
	if persisted.Password != "" {
		t.Fatalf("persisted password = %q, want empty (secret leaked to disk)", persisted.Password)
	}
	if persisted.Username != "nologin" || persisted.PasswordEnv != "RG_PASS" {
		t.Fatalf("persisted auth = %+v, want username+PasswordEnv preserved", persisted)
	}

	// Belt and braces: the secret must not appear anywhere in the raw bytes.
	raw, err := os.ReadFile(filepath.Join(home, "sklp", "spaces", "production", "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if bytes.Contains(raw, []byte("s3cr3t-shipped")) {
		t.Fatalf("secret found in manifest.json:\n%s", raw)
	}
}

// TestApplyDoesNotAliasCallerRegistries: stripping the password for disk must
// not mutate the spec the live supervisor is still using. A shallow map copy
// would alias and blank the in-memory credential.
func TestApplyDoesNotAliasCallerRegistries(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	spec := applySpec{
		Services: []spacemanager.Service{{Name: "core", Image: "img:1"}},
		Meta: spacemanager.SpaceMeta{
			Name: "production",
			Registries: map[string]spacemanager.RegistryAuthMeta{
				"rg.example.com": {Username: "nologin", Password: "s3cr3t-shipped"},
			},
		},
	}
	if err := persistManifest(spec); err != nil {
		t.Fatalf("persistManifest: %v", err)
	}
	if got := spec.Meta.Registries["rg.example.com"].Password; got != "s3cr3t-shipped" {
		t.Fatalf("caller's password = %q, want it untouched (aliased write)", got)
	}
}

// TestApplyUnchangedDoesNotPersist: an apply that changes nothing writes
// nothing — no needless disk churn on a repeated identical deploy.
func TestApplyUnchangedDoesNotPersist(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)

	sup := &fakeSupervisor{
		live: map[string]spacemanager.Service{"core": {Name: "core", Image: "img:1", DeclaredImage: "img:1"}},
	}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	spec, _ := MarshalApplySpec(
		[]spacemanager.Service{{Name: "core", Image: "img:1"}},
		spacemanager.SpaceMeta{Name: "production"},
	)
	res, err := c.Apply(context.Background(), spec)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Result != ResultUnchanged {
		t.Fatalf("result = %q, want unchanged", res.Result)
	}
	if _, err := os.Stat(filepath.Join(home, "sklp", "spaces", "production", "manifest.json")); !os.IsNotExist(err) {
		t.Fatalf("unchanged apply wrote a manifest (stat err = %v)", err)
	}
}

// TestPersistManifestRejectsNamelessSpace: without a space name there is no
// deterministic path to write to; persisting must fail rather than guess.
func TestPersistManifestRejectsNamelessSpace(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	err := persistManifest(applySpec{
		Services: []spacemanager.Service{{Name: "core", Image: "img:1"}},
	})
	if err == nil {
		t.Fatal("persistManifest with no space name returned nil, want an error")
	}
}

func TestApplyFromSourceResolvesLikeTheCLI(t *testing.T) {
	sup := &fakeSupervisor{live: map[string]spacemanager.Service{}, differs: map[string]bool{}, routesChanged: true}
	c, cancel := serveFake(t, "app-production", sup)
	defer cancel()

	src := ApplySource{
		Stack: "prod",
		Vars:  map[string]string{"APP_HOST": "example.org", "DB_PASS": "p4ss"},
		YAML: `
services:
  postgres:
    image: postgres:17-alpine
    port: 5432
    env:
      POSTGRES_PASSWORD: ${DB_PASS}
    volumes: ["pgdata:/var/lib/postgresql/data"]
  web:
    image: img:1
    port: 5273
jobs:
  migrate:
    image: migrate:1
    command: ["postgres://app:${DB_PASS}@postgres:5432/app", "up"]
spaces:
  app-production:
    gateway:
      routes:
        - service: web
          host: ${APP_HOST}
`,
	}
	res, err := c.ApplySource(context.Background(), src, false)
	if err != nil {
		t.Fatalf("apply source: %v", err)
	}
	if res.Result != ResultCreated {
		t.Fatalf("result = %q, want created", res.Result)
	}
	if len(sup.added) != 3 {
		t.Fatalf("added = %v, want postgres, web, migrate", sup.added)
	}
	if len(sup.routesSet) != 1 || sup.routesSet[0].Host != "example.org" {
		t.Fatalf("routes = %+v, want the ${APP_HOST} route resolved", sup.routesSet)
	}
	var migrate, postgres spacemanager.Service
	for _, s := range sup.live {
		switch s.Name {
		case "migrate":
			migrate = s
		case "postgres":
			postgres = s
		}
	}
	if !migrate.Oneshot || migrate.Command[0] != "postgres://app:p4ss@postgres:5432/app" {
		t.Fatalf("migrate = %+v", migrate)
	}
	if len(postgres.Volumes) != 1 {
		t.Fatalf("postgres volumes = %v", postgres.Volumes)
	}
}

func TestApplyFromSourceUnresolvedVarIsRefused(t *testing.T) {
	sup := &fakeSupervisor{live: map[string]spacemanager.Service{}, differs: map[string]bool{}}
	c, cancel := serveFake(t, "app-production", sup)
	defer cancel()

	src := ApplySource{YAML: "services:\n  web:\n    image: img:1\n    env:\n      K: ${MISSING}\nspaces:\n  app-production: {}\n"}
	_, err := c.ApplySource(context.Background(), src, false)
	if err == nil || !strings.Contains(err.Error(), "MISSING") {
		t.Fatalf("err = %v, want the unresolved variable named", err)
	}
	if len(sup.added) != 0 {
		t.Fatalf("a refused source must not touch the supervisor: added %v", sup.added)
	}
}

// TestApplyKeepsRolledImageWhenGitUnchanged: a manifest that still declares the
// tag a service was first deployed with, while an image watch has since rolled
// it further, keeps the rolled image and records the declaration.
func TestApplyKeepsRolledImageWhenGitUnchanged(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	sup := &fakeSupervisor{
		live: map[string]spacemanager.Service{
			"core": {Name: "core", Image: "img:2", DeclaredImage: "img:1"},
			"new":  {Name: "new", Image: "new:1", DeclaredImage: "new:1"},
		},
		differs: map[string]bool{"core": true, "new": true},
	}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	spec, _ := MarshalApplySpec([]spacemanager.Service{
		{Name: "core", Image: "img:1"},
		{Name: "new", Image: "new:1"},
		{Name: "job", Image: "job:1"},
	}, spacemanager.SpaceMeta{Name: "production"})
	res, err := c.Apply(context.Background(), spec)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Result != ResultConfigured {
		t.Fatalf("result = %q, want configured", res.Result)
	}
	if got := sup.live["core"].Image; got != "img:2" {
		t.Fatalf("core reconfigured to %q, want the rolled img:2 kept", got)
	}
	services, _, err := ReadPersistedManifest("production")
	if err != nil {
		t.Fatal(err)
	}
	for _, svc := range services {
		if svc.Name == "core" && (svc.Image != "img:2" || svc.DeclaredImage != "img:1") {
			t.Fatalf("persisted core = image %q declared %q, want img:2 / img:1", svc.Image, svc.DeclaredImage)
		}
	}
}

// TestApplyGitTagChangeWins: a tag edited in Git is a decision, not a stale
// declaration; it overrides whatever a watch rolled.
func TestApplyGitTagChangeWins(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	sup := &fakeSupervisor{
		live:    map[string]spacemanager.Service{"core": {Name: "core", Image: "img:2", DeclaredImage: "img:1"}},
		differs: map[string]bool{"core": true},
	}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	spec, _ := MarshalApplySpec([]spacemanager.Service{{Name: "core", Image: "img:3"}}, spacemanager.SpaceMeta{Name: "production"})
	if _, err := c.Apply(context.Background(), spec); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := sup.live["core"]; got.Image != "img:3" || got.DeclaredImage != "img:3" {
		t.Fatalf("core = image %q declared %q, want img:3 / img:3", got.Image, got.DeclaredImage)
	}
}

// TestApplyLegacyManifestWithoutDeclaredImage: a service deployed before the
// declaration existed carries none; the incoming tag applies as it always did.
func TestApplyLegacyManifestWithoutDeclaredImage(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	sup := &fakeSupervisor{
		live:    map[string]spacemanager.Service{"core": {Name: "core", Image: "img:2"}},
		differs: map[string]bool{"core": true},
	}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	spec, _ := MarshalApplySpec([]spacemanager.Service{{Name: "core", Image: "img:1"}}, spacemanager.SpaceMeta{Name: "production"})
	if _, err := c.Apply(context.Background(), spec); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := sup.live["core"].Image; got != "img:1" {
		t.Fatalf("core = %q, want img:1 applied verbatim", got)
	}
}

// TestApplyRejectedSpecIsReported: a spec the supervisor just rolled back is
// refused with its reason, nothing of that service moves, and the persisted
// manifest keeps naming the image that runs.
func TestApplyRejectedSpecIsReported(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	sup := &fakeSupervisor{
		live: map[string]spacemanager.Service{
			"core": {Name: "core", Image: "img:1", DeclaredImage: "img:1"},
			"web":  {Name: "web", Image: "web:1", DeclaredImage: "web:1"},
		},
		differs: map[string]bool{"core": true, "web": true},
		rejects: map[string]string{"core": "exited 1 before ready"},
	}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	spec, _ := MarshalApplySpec([]spacemanager.Service{
		{Name: "core", Image: "img:2"},
		{Name: "web", Image: "web:2"},
	}, spacemanager.SpaceMeta{Name: "production"})
	res, err := c.Apply(context.Background(), spec)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Result != ResultRejected {
		t.Fatalf("result = %q, want rejected", res.Result)
	}
	if len(sup.reconfigured) != 1 || sup.reconfigured[0] != "web" {
		t.Fatalf("reconfigured = %v, want only [web]", sup.reconfigured)
	}
	if !strings.Contains(strings.Join(res.Changes, "\n"), "rejected service core: exited 1 before ready") {
		t.Fatalf("changes = %v, want the rejection with its reason", res.Changes)
	}
	services, _, err := ReadPersistedManifest("production")
	if err != nil {
		t.Fatal(err)
	}
	for _, svc := range services {
		switch svc.Name {
		case "core":
			if svc.Image != "img:1" {
				t.Fatalf("persisted core = %q, want the running img:1, not the rejected tag", svc.Image)
			}
		case "web":
			if svc.Image != "web:2" {
				t.Fatalf("persisted web = %q, want web:2", svc.Image)
			}
		}
	}
}

// TestApplyOnlyRejectionPersistsNothing: a re-apply whose sole difference is a
// rejected spec mutates nothing and writes no manifest.
func TestApplyOnlyRejectionPersistsNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	sup := &fakeSupervisor{
		live:    map[string]spacemanager.Service{"core": {Name: "core", Image: "img:1", DeclaredImage: "img:1"}},
		differs: map[string]bool{"core": true},
		rejects: map[string]string{"core": "exited 1 before ready"},
	}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	spec, _ := MarshalApplySpec([]spacemanager.Service{{Name: "core", Image: "img:2"}}, spacemanager.SpaceMeta{Name: "production"})
	res, err := c.Apply(context.Background(), spec)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Result != ResultRejected || len(sup.reconfigured) != 0 {
		t.Fatalf("result = %q reconfigured = %v, want rejected and no mutation", res.Result, sup.reconfigured)
	}
	if _, err := os.Stat(filepath.Join(home, "sklp", "spaces", "production", "manifest.json")); !os.IsNotExist(err) {
		t.Fatalf("a rejected-only apply wrote a manifest (stat err = %v)", err)
	}
}

// TestApplyRecordsDeclaredImageWithoutRestart: a manifest whose tags already
// match what runs records the declaration for every service and persists it,
// without reconfiguring anything; the next identical apply is unchanged.
func TestApplyRecordsDeclaredImageWithoutRestart(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	sup := &fakeSupervisor{
		live: map[string]spacemanager.Service{
			"core": {Name: "core", Image: "img:2"},
			"web":  {Name: "web", Image: "web:1"},
		},
	}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	spec, _ := MarshalApplySpec([]spacemanager.Service{
		{Name: "core", Image: "img:2"},
		{Name: "web", Image: "web:1"},
	}, spacemanager.SpaceMeta{Name: "production"})
	res, err := c.Apply(context.Background(), spec)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Result != ResultConfigured || len(sup.reconfigured) != 0 {
		t.Fatalf("result = %q reconfigured = %v, want configured with no reconfigure", res.Result, sup.reconfigured)
	}
	if len(sup.declared) != 2 || !strings.Contains(strings.Join(res.Changes, "\n"), "recorded declared image for core") {
		t.Fatalf("declared = %v changes = %v", sup.declared, res.Changes)
	}
	services, _, err := ReadPersistedManifest("production")
	if err != nil {
		t.Fatal(err)
	}
	for _, svc := range services {
		if svc.DeclaredImage != svc.Image {
			t.Fatalf("persisted %s = image %q declared %q, want them equal", svc.Name, svc.Image, svc.DeclaredImage)
		}
	}

	res, err = c.Apply(context.Background(), spec)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if res.Result != ResultUnchanged {
		t.Fatalf("second apply = %q, want unchanged", res.Result)
	}
}

// TestApplyRecordsDeclarationAfterMerge: when the merge keeps a rolled image,
// the new declaration is still recorded, so the next manifest-only push merges
// against it.
func TestApplyRecordsDeclarationAfterMerge(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	sup := &fakeSupervisor{
		live: map[string]spacemanager.Service{"core": {Name: "core", Image: "img:2", DeclaredImage: "img:1"}},
	}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	spec, _ := MarshalApplySpec([]spacemanager.Service{{Name: "core", Image: "img:1"}}, spacemanager.SpaceMeta{Name: "production"})
	res, err := c.Apply(context.Background(), spec)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Result != ResultUnchanged || len(sup.declared) != 0 {
		t.Fatalf("same declaration: result = %q declared = %v, want unchanged", res.Result, sup.declared)
	}
}

// TestApplyStaleGitTagKeepsRolledImage: shop 2026-10-07. Git bumped the tag
// while every reconcile was refused upstream, then rolls moved the service
// further; the bump finally landing is older than what runs and is not a
// decision.
func TestApplyStaleGitTagKeepsRolledImage(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	sup := &fakeSupervisor{
		live: map[string]spacemanager.Service{
			"web": {Name: "web", Image: "img:4", DeclaredImage: "img:1", RolledImages: []string{"img:2", "img:3", "img:4"}},
		},
		differs: map[string]bool{"web": true},
	}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	spec, _ := MarshalApplySpec([]spacemanager.Service{{Name: "web", Image: "img:3"}}, spacemanager.SpaceMeta{Name: "production"})
	if _, err := c.Apply(context.Background(), spec); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := sup.live["web"]; got.Image != "img:4" || got.DeclaredImage != "img:3" {
		t.Fatalf("web = image %q declared %q, want img:4 / img:3", got.Image, got.DeclaredImage)
	}
	services, _, err := ReadPersistedManifest("production")
	if err != nil {
		t.Fatal(err)
	}
	if len(services) != 1 || len(services[0].RolledImages) != 3 {
		t.Fatalf("persisted journal = %v, want the three rolled tags kept", services[0].RolledImages)
	}
}

// TestApplyUnseenGitTagResetsJournal: a tag no roll ever passed is Git's
// decision; it applies and the journal starts over.
func TestApplyUnseenGitTagResetsJournal(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	sup := &fakeSupervisor{
		live: map[string]spacemanager.Service{
			"web": {Name: "web", Image: "img:4", DeclaredImage: "img:1", RolledImages: []string{"img:2", "img:3", "img:4"}},
		},
		differs: map[string]bool{"web": true},
	}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	spec, _ := MarshalApplySpec([]spacemanager.Service{{Name: "web", Image: "img:9"}}, spacemanager.SpaceMeta{Name: "production"})
	if _, err := c.Apply(context.Background(), spec); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := sup.live["web"]; got.Image != "img:9" || len(got.RolledImages) != 0 {
		t.Fatalf("web = image %q journal %v, want img:9 with an empty journal", got.Image, got.RolledImages)
	}
}
