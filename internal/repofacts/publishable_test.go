package repofacts

import (
	"testing"
)

// ── npm ──────────────────────────────────────────────────────

func TestPublishable_NpmScopedPublic(t *testing.T) {
	f, err := Scan(mkRepo(t, map[string]string{
		"packages/sdk/package.json": `{
			"name": "@acme/sdk",
			"version": "1.2.3",
			"publishConfig": { "registry": "https://registry.npmjs.org" }
		}`,
	}))
	if err != nil {
		t.Fatal(err)
	}
	got := f.PublishableArtifacts.NpmPackages
	if len(got) != 1 {
		t.Fatalf("expected 1 npm pkg, got %d", len(got))
	}
	pkg := got[0]
	if pkg.Name != "@acme/sdk" {
		t.Errorf("name = %q", pkg.Name)
	}
	if pkg.Version != "1.2.3" {
		t.Errorf("version = %q (the bump-detection cue)", pkg.Version)
	}
	if pkg.Private {
		t.Errorf("public pkg flagged private")
	}
	if pkg.Registry != "https://registry.npmjs.org" {
		t.Errorf("registry = %q", pkg.Registry)
	}
}

func TestPublishable_NpmPrivateSurfacedNotSilent(t *testing.T) {
	f, err := Scan(mkRepo(t, map[string]string{
		"package.json": `{"name":"internal","version":"0.1.0","private":true}`,
	}))
	if err != nil {
		t.Fatal(err)
	}
	got := f.PublishableArtifacts.NpmPackages
	if len(got) != 1 || !got[0].Private {
		t.Fatalf("private package must be surfaced with Private=true, got %+v", got)
	}
}

func TestPublishable_NpmRegistryFromNpmrc(t *testing.T) {
	// .npmrc resolves the scope → registry mapping when
	// publishConfig.registry is absent.
	f, err := Scan(mkRepo(t, map[string]string{
		"packages/sdk/package.json": `{"name":"@example-org/sdk","version":"0.1.0"}`,
		"packages/sdk/.npmrc":       "@example-org:registry=https://npm.pkg.github.com\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.PublishableArtifacts.NpmPackages; len(got) != 1 || got[0].Registry != "https://npm.pkg.github.com" {
		t.Errorf(".npmrc scoped registry not resolved: %+v", got)
	}
}

func TestPublishable_NpmIgnoresNodeModules(t *testing.T) {
	f, err := Scan(mkRepo(t, map[string]string{
		"package.json":                  `{"name":"root","version":"1.0.0"}`,
		"node_modules/lib/package.json": `{"name":"lib","version":"5.0.0"}`,
	}))
	if err != nil {
		t.Fatal(err)
	}
	got := f.PublishableArtifacts.NpmPackages
	if len(got) != 1 || got[0].Name != "root" {
		t.Errorf("node_modules leaked: %+v", got)
	}
}

// ── cargo ────────────────────────────────────────────────────

func TestPublishable_CargoPublishableCrate(t *testing.T) {
	f, err := Scan(mkRepo(t, map[string]string{
		"Cargo.toml": `[package]
name = "acme"
version = "0.4.2"
edition = "2021"

[dependencies]
serde = "1"
`,
	}))
	if err != nil {
		t.Fatal(err)
	}
	got := f.PublishableArtifacts.CargoCrates
	if len(got) != 1 {
		t.Fatalf("expected 1 crate, got %d", len(got))
	}
	if got[0].Name != "acme" || got[0].Version != "0.4.2" {
		t.Errorf("bad name/version: %+v", got[0])
	}
	if !got[0].Publish {
		t.Errorf("default cargo crate should be Publish=true")
	}
}

func TestPublishable_CargoPublishFalseSkipped(t *testing.T) {
	f, err := Scan(mkRepo(t, map[string]string{
		"Cargo.toml": `[package]
name = "internal"
version = "0.1.0"
publish = false
`,
	}))
	if err != nil {
		t.Fatal(err)
	}
	got := f.PublishableArtifacts.CargoCrates
	if len(got) != 1 || got[0].Publish {
		t.Fatalf("publish=false should yield Publish=false, got %+v", got)
	}
}

func TestPublishable_CargoCustomRegistry(t *testing.T) {
	f, err := Scan(mkRepo(t, map[string]string{
		"Cargo.toml": `[package]
name = "acme"
version = "1.0.0"
publish = ["my-registry"]
`,
	}))
	if err != nil {
		t.Fatal(err)
	}
	got := f.PublishableArtifacts.CargoCrates
	if len(got) != 1 || got[0].Registry != "my-registry" {
		t.Errorf("custom registry not captured: %+v", got)
	}
}

func TestPublishable_CargoVirtualWorkspaceSkipped(t *testing.T) {
	// A workspace root without [package] is not itself publishable.
	f, err := Scan(mkRepo(t, map[string]string{
		"Cargo.toml": `[workspace]
members = ["crates/*"]
`,
		"crates/foo/Cargo.toml": `[package]
name = "foo"
version = "0.1.0"
`,
	}))
	if err != nil {
		t.Fatal(err)
	}
	got := f.PublishableArtifacts.CargoCrates
	if len(got) != 1 || got[0].Name != "foo" {
		t.Errorf("virtual workspace should be skipped, only foo should remain: %+v", got)
	}
}

// ── python ───────────────────────────────────────────────────

func TestPublishable_PythonPEP621(t *testing.T) {
	f, err := Scan(mkRepo(t, map[string]string{
		"pyproject.toml": `[build-system]
requires = ["hatchling"]
build-backend = "hatchling.build"

[project]
name = "acme"
version = "0.3.1"
`,
	}))
	if err != nil {
		t.Fatal(err)
	}
	got := f.PublishableArtifacts.PythonPackages
	if len(got) != 1 {
		t.Fatalf("expected 1 py pkg, got %d", len(got))
	}
	pkg := got[0]
	if pkg.Name != "acme" || pkg.Version != "0.3.1" {
		t.Errorf("name/version: %+v", pkg)
	}
	if pkg.Backend != "hatchling.build" {
		t.Errorf("backend = %q (drives `hatch publish` vs `twine upload`)", pkg.Backend)
	}
}

func TestPublishable_PythonPoetry(t *testing.T) {
	f, err := Scan(mkRepo(t, map[string]string{
		"pyproject.toml": `[tool.poetry]
name = "acme"
version = "1.4.0"

[build-system]
requires = ["poetry-core"]
build-backend = "poetry.core.masonry.api"
`,
	}))
	if err != nil {
		t.Fatal(err)
	}
	got := f.PublishableArtifacts.PythonPackages
	if len(got) != 1 || got[0].Backend != "poetry.core.masonry.api" {
		t.Errorf("poetry backend not picked: %+v", got)
	}
}

// ── helm ─────────────────────────────────────────────────────

func TestPublishable_HelmChart(t *testing.T) {
	f, err := Scan(mkRepo(t, map[string]string{
		"charts/platform/Chart.yaml": `apiVersion: v2
name: platform
description: Platform umbrella chart
version: 1.5.0
appVersion: "2.10.3"
`,
		"charts/platform/values.yaml":      "replicaCount: 1\n",
		"charts/platform/templates/x.yaml": "kind: Pod\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	got := f.PublishableArtifacts.HelmCharts
	if len(got) != 1 {
		t.Fatalf("expected 1 chart, got %d", len(got))
	}
	chart := got[0]
	if chart.Name != "platform" || chart.Version != "1.5.0" || chart.AppVersion != "2.10.3" {
		t.Errorf("chart metadata wrong: %+v", chart)
	}
}

// ── go SDK ──────────────────────────────────────────────────

func TestPublishable_GoSDKModulePublic(t *testing.T) {
	// Dir name 'sdk-go' + public module path = SDK candidate.
	f, err := Scan(mkRepo(t, map[string]string{
		"packages/sdk-go/go.mod": "module github.com/example-org/sdk-go\n\ngo 1.25.0\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	got := f.PublishableArtifacts.GoModules
	if len(got) != 1 {
		t.Fatalf("expected 1 go module, got %d", len(got))
	}
	if !got[0].Public {
		t.Errorf("github.com/... should be marked Public, got %+v", got[0])
	}
}

func TestPublishable_GoInternalModuleSkipped(t *testing.T) {
	// services/api is NOT an SDK — heuristic should skip it.
	f, err := Scan(mkRepo(t, map[string]string{
		"services/api/go.mod": "module github.com/example-org/app/services/api\n\ngo 1.25.0\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.PublishableArtifacts.GoModules; len(got) != 0 {
		t.Errorf("services/api should not be surfaced as publishable: %+v", got)
	}
}

func TestPublishable_GoInternalPathOverridesPublicHost(t *testing.T) {
	// Even with github.com/ host, /internal/ marker forbids publish.
	f, err := Scan(mkRepo(t, map[string]string{
		"packages/sdk-internal/go.mod": "module github.com/example-org/internal/sdk-internal\n\ngo 1.25.0\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	got := f.PublishableArtifacts.GoModules
	if len(got) == 0 {
		t.Fatal("module should still be surfaced (with Public=false)")
	}
	if got[0].Public {
		t.Errorf("/internal/ path should force Public=false, got %+v", got[0])
	}
}

// ── mixed monorepo ──────────────────────────────────────────

func TestPublishable_MixedMonorepo(t *testing.T) {
	// A realistic bakery-like layout: Go SDK + npm SDK + helm chart.
	f, err := Scan(mkRepo(t, map[string]string{
		"packages/sdk-go/go.mod":       "module github.com/example-org/sdk-go\n\ngo 1.25.0\n",
		"packages/sdk-js/package.json": `{"name":"@example-org/sdk","version":"0.5.0"}`,
		"charts/platform/Chart.yaml":   "apiVersion: v2\nname: platform\nversion: 0.1.0\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	pa := f.PublishableArtifacts
	if len(pa.GoModules) != 1 || len(pa.NpmPackages) != 1 || len(pa.HelmCharts) != 1 {
		t.Errorf("mixed monorepo not fully detected: %+v", pa)
	}
}
