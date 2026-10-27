package control

import (
	"context"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/spacemanager"
)

// seedManifest persists a one-service manifest for space and isolates the
// config root, so a test drives the same LoadManifest path the handler uses.
func seedManifest(t *testing.T, space, svc, image string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	services := []spacemanager.Service{{Name: svc, Image: image, DeclaredImage: image, Port: 8080}}
	meta := spacemanager.SpaceMeta{Name: space}
	if err := SaveManifest(space, services, meta); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}
}

// TestSetImageRollsService: a roll onto a new tag reconfigures exactly that
// service and persists the new image, so an agent restart resurrects the rolled
// workload rather than the tag it was first deployed with.
func TestSetImageRollsService(t *testing.T) {
	seedManifest(t, "production", "web", "rg.example.com/web:1")
	sup := &fakeSupervisor{
		live:    map[string]spacemanager.Service{"web": {Name: "web", Image: "rg.example.com/web:1"}},
		differs: map[string]bool{"web": true},
	}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	res, err := c.SetImage(context.Background(), "web", "rg.example.com/web:2")
	if err != nil {
		t.Fatalf("set image: %v", err)
	}
	if res.Result != ResultConfigured {
		t.Fatalf("result = %q, want %q", res.Result, ResultConfigured)
	}
	if len(sup.reconfigured) != 1 || sup.reconfigured[0] != "web" {
		t.Fatalf("reconfigured = %v, want exactly [web]", sup.reconfigured)
	}

	services, _, err := LoadManifest("production")
	if err != nil {
		t.Fatalf("reload manifest: %v", err)
	}
	if services[0].Image != "rg.example.com/web:2" {
		t.Fatalf("persisted image = %q, want the rolled tag", services[0].Image)
	}
	if services[0].DeclaredImage != "rg.example.com/web:1" {
		t.Fatalf("declared image = %q after a roll, want the one the last apply declared", services[0].DeclaredImage)
	}
}

// TestSetImageSameTagUnchanged: re-posting the tag a service already runs is a
// no-op. A watcher re-checking a registry must not bounce a healthy service.
func TestSetImageSameTagUnchanged(t *testing.T) {
	seedManifest(t, "production", "web", "rg.example.com/web:1")
	sup := &fakeSupervisor{
		live: map[string]spacemanager.Service{"web": {Name: "web", Image: "rg.example.com/web:1"}},
	}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	res, err := c.SetImage(context.Background(), "web", "rg.example.com/web:1")
	if err != nil {
		t.Fatalf("set image: %v", err)
	}
	if res.Result != ResultUnchanged {
		t.Fatalf("result = %q, want %q", res.Result, ResultUnchanged)
	}
	if len(sup.reconfigured) != 0 {
		t.Fatalf("Reconfigure called on an unchanged roll: %+v", sup.reconfigured)
	}
}

// TestSetImageUnknownService: naming a service the space does not run is an
// error, not a silent addition — a roll changes what exists, it never creates.
func TestSetImageUnknownService(t *testing.T) {
	seedManifest(t, "production", "web", "rg.example.com/web:1")
	sup := &fakeSupervisor{}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	_, err := c.SetImage(context.Background(), "api", "rg.example.com/api:1")
	if err == nil {
		t.Fatal("expected an error for a service absent from the manifest")
	}
	if !strings.Contains(err.Error(), "unknown service") {
		t.Fatalf("error = %v, want it to name the unknown service", err)
	}
}

// TestValidImageRef bounds what a caller may steer the pull toward: a reference
// carrying shell metacharacters or no tag has no legitimate form.
func TestValidImageRef(t *testing.T) {
	valid := []string{
		"rg.example.com/ns/web:20260820-1349-bff7534",
		"debian:trixie-slim",
		"rg.fr-par.scw.cloud/altfab/www:1.0.0",
	}
	for _, ref := range valid {
		if err := validImageRef(ref); err != nil {
			t.Errorf("validImageRef(%q) = %v, want nil", ref, err)
		}
	}
	invalid := []string{
		"",
		"web",                         // untagged: a floating tag is not deployable
		"web:latest; rm -rf /",        // shell metacharacters
		"web:$(whoami)",               // command substitution
		"rg.example.com/web:1 --flag", // whitespace
	}
	for _, ref := range invalid {
		if err := validImageRef(ref); err == nil {
			t.Errorf("validImageRef(%q) = nil, want an error", ref)
		}
	}
}

// A roll rebuilds the desired set from the on-disk manifest, whose templated
// values are expanded from the space env. Persisting the secrets that apply
// "resolved" must not shrink the file: a var referenced only inside a
// composed value has no whole-value entry to be re-collected from, and losing
// it left the next roll expanding "https://${HOST}" to "https://" (partage,
// 2026-09-04).
func TestSetImageKeepsTemplateVarsInSpaceEnv(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := persistManifest(applySpec{
		Services: []spacemanager.Service{{Name: "web", Image: "rg.example.com/web:1", Env: []string{"AUTH_URL=https://app.example", "DB_PASS=s3"}}},
		Meta: spacemanager.SpaceMeta{
			Name:      "production",
			EnvRefs:   map[string]string{"AUTH_URL": "https://${HOST}", "DB_PASS": "${DB_PASS}"},
			EnvValues: map[string]string{"HOST": "app.example", "DB_PASS": "s3"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := WriteSpaceEnv("production", map[string]string{"HOST": "app.example", "DB_PASS": "s3"}); err != nil {
		t.Fatal(err)
	}
	sup := &fakeSupervisor{
		live:    map[string]spacemanager.Service{"web": {Name: "web", Image: "rg.example.com/web:1"}},
		differs: map[string]bool{"web": true},
	}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	if _, err := c.SetImage(context.Background(), "web", "rg.example.com/web:2"); err != nil {
		t.Fatalf("set image: %v", err)
	}

	env, err := ReadSpaceEnv("production")
	if err != nil {
		t.Fatal(err)
	}
	if env["HOST"] != "app.example" || env["DB_PASS"] != "s3" {
		t.Fatalf("space env after the roll = %v, want HOST and DB_PASS kept", env)
	}
	services, _, err := LoadManifest("production")
	if err != nil {
		t.Fatal(err)
	}
	if got := services[0].Env[0]; got != "AUTH_URL=https://app.example" {
		t.Fatalf("composed value after the roll = %q", got)
	}
}

// TestSetImageComparesAgainstRunningImage: the manifest on disk may name a tag
// that was rolled back; a roll onto it is judged against what runs, so the
// watcher gets it applied rather than "unchanged".
func TestSetImageComparesAgainstRunningImage(t *testing.T) {
	seedManifest(t, "production", "web", "rg.example.com/web:2")
	sup := &fakeSupervisor{
		live:    map[string]spacemanager.Service{"web": {Name: "web", Image: "rg.example.com/web:1"}},
		differs: map[string]bool{"web": true},
	}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	res, err := c.SetImage(context.Background(), "web", "rg.example.com/web:2")
	if err != nil {
		t.Fatalf("set image: %v", err)
	}
	if res.Result != ResultConfigured || len(sup.reconfigured) != 1 {
		t.Fatalf("result = %q reconfigured = %v, want configured and one reconfigure", res.Result, sup.reconfigured)
	}
}

// TestSetImageRejectedTag: rolling onto the tag the supervisor just rolled back
// is refused with the rollback's reason; the manifest keeps the running image.
func TestSetImageRejectedTag(t *testing.T) {
	seedManifest(t, "production", "web", "rg.example.com/web:1")
	sup := &fakeSupervisor{
		live:    map[string]spacemanager.Service{"web": {Name: "web", Image: "rg.example.com/web:1"}},
		differs: map[string]bool{"web": true},
		rejects: map[string]string{"web": "exited 1 before ready"},
	}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	res, err := c.SetImage(context.Background(), "web", "rg.example.com/web:2")
	if err != nil {
		t.Fatalf("set image: %v", err)
	}
	if res.Result != ResultRejected {
		t.Fatalf("result = %q, want rejected", res.Result)
	}
	if len(sup.reconfigured) != 0 {
		t.Fatalf("Reconfigure called on a rejected roll: %v", sup.reconfigured)
	}
	if !strings.Contains(strings.Join(res.Changes, "\n"), "exited 1 before ready") {
		t.Fatalf("changes = %v, want the rollback reason", res.Changes)
	}
	services, _, err := LoadManifest("production")
	if err != nil {
		t.Fatal(err)
	}
	if services[0].Image != "rg.example.com/web:1" {
		t.Fatalf("persisted image = %q, want the running one", services[0].Image)
	}
}

// TestSetImageDoesNotMergeDeclaredImage: a roll that deliberately targets the
// tag Git declared applies it; the three-way merge belongs to full applies only.
func TestSetImageDoesNotMergeDeclaredImage(t *testing.T) {
	seedManifest(t, "production", "web", "rg.example.com/web:2")
	sup := &fakeSupervisor{
		live:    map[string]spacemanager.Service{"web": {Name: "web", Image: "rg.example.com/web:2", DeclaredImage: "rg.example.com/web:1"}},
		differs: map[string]bool{"web": true},
	}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	if _, err := c.SetImage(context.Background(), "web", "rg.example.com/web:1"); err != nil {
		t.Fatalf("set image: %v", err)
	}
	if got := sup.live["web"].Image; got != "rg.example.com/web:1" {
		t.Fatalf("web = %q, want the requested web:1", got)
	}
}

// TestSetImageJournalsRolledTag: every roll is remembered so a later apply can
// tell a stale Git tag from a new one.
func TestSetImageJournalsRolledTag(t *testing.T) {
	seedManifest(t, "production", "web", "rg.example.com/web:1")
	sup := &fakeSupervisor{
		live:    map[string]spacemanager.Service{"web": {Name: "web", Image: "rg.example.com/web:1"}},
		differs: map[string]bool{"web": true},
	}
	c, cancel := serveFake(t, "production", sup)
	defer cancel()

	for _, tag := range []string{"rg.example.com/web:2", "rg.example.com/web:3"} {
		if _, err := c.SetImage(context.Background(), "web", tag); err != nil {
			t.Fatalf("set image %s: %v", tag, err)
		}
		sup.live["web"] = spacemanager.Service{Name: "web", Image: tag}
	}
	services, _, err := LoadManifest("production")
	if err != nil {
		t.Fatalf("reload manifest: %v", err)
	}
	want := []string{"rg.example.com/web:2", "rg.example.com/web:3"}
	if got := services[0].RolledImages; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("journal = %v, want %v", got, want)
	}
}
