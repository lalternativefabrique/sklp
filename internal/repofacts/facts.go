// Package repofacts is a deterministic repository scanner. Given a
// path on disk, it produces a structured RepoFacts record describing
// what's actually present: which languages, which modules / packages
// / workspaces, which Dockerfiles, which Makefile targets, and a
// best-effort registry URL.
//
// Why a separate package instead of letting the LLM discover all this
// via list_dir / read_file:
//
//   - Determinism. A glob walk + manifest parser will never miss a
//     go.mod under apps/<svc>/. A LLM might (we observed this on the
//     bakery repo). The init command should not rely on the model's
//     attention budget for facts the filesystem already has.
//   - Speed. The whole scan is sub-100ms on a 5-service monorepo;
//     the LLM equivalent is ~10 tool calls and 30s of inference.
//   - Reuse. sklp diagnose, sklp investigate, and a future Skalpel
//     "auto-onboard a project" flow all want the same facts. A
//     dedicated package lets each caller decide how to consume them.
//
// The output is JSON-serialisable on purpose: the consumer most often
// embeds it verbatim into an LLM system prompt as ground truth.
package repofacts

// RepoFacts is the structured view of a repository's surface area.
// Every field is optional — an empty struct is a valid result for a
// freshly initialised repo with no manifests.
type RepoFacts struct {
	// Root is the absolute path that was scanned. Echoed back in the
	// struct so callers serialising RepoFacts have it without keeping
	// a separate variable in scope.
	Root string `json:"root"`

	// Languages enumerates each language stack the scan found,
	// alongside the modules / packages / workspaces specific to it.
	// Order is deterministic (sorted by Lang field).
	Languages []LanguageFacts `json:"languages,omitempty"`

	// Dockerfiles lists every Dockerfile-like file at any depth. The
	// path is relative to Root with forward slashes.
	//
	// Recognised filenames: Dockerfile, Dockerfile.<svc>,
	// Containerfile. Compose files are NOT in this list (they are
	// covered by ComposeFiles below).
	Dockerfiles []string `json:"dockerfiles,omitempty"`

	// ComposeFiles lists every compose.yml / docker-compose.yml at any
	// depth. Helps the LLM understand local development topology.
	ComposeFiles []string `json:"compose_files,omitempty"`

	// Makefile, when present at the repo root, exposes the build glue
	// the LLM should prefer over inventing commands. We deliberately
	// only look at the root Makefile — nested Makefiles are too varied
	// to parse reliably and are usually called from the root one.
	Makefile *MakefileFacts `json:"makefile,omitempty"`

	// Registry, when non-empty, is the best-effort container registry
	// the repo seems to publish to (e.g. rg.fr-par.scw.cloud/bakery,
	// ghcr.io/myorg). Source-of-truth precedence: Makefile REGISTRY
	// variable > docker-compose image tag > GitHub Actions workflow.
	// Empty means "no inference made, ask the user".
	Registry string `json:"registry,omitempty"`

	// CIHints surfaces existing CI definitions the LLM can mine for
	// commands. We DO NOT recommend translating these to a sklp
	// pipeline blindly — they're a hint about which test/build
	// commands are conventional in this repo.
	CIHints CIHints `json:"ci_hints,omitempty"`

	// APIContracts groups every machine-readable API description the
	// scan found. Drives "regenerate typed client" and "breaking
	// change check" CI steps. A repo can carry several contracts at
	// once (OpenAPI for the public HTTP surface + Protobuf for an
	// internal gRPC service) — each kind is listed independently so
	// the LLM can emit one step per kind.
	APIContracts APIContracts `json:"api_contracts,omitempty"`

	// PublishableArtifacts lists everything this repo SHIPS aside
	// from Docker images (those are inferred from Dockerfiles +
	// Registry). One repo can ship several artifacts of different
	// kinds — a Go monorepo with a JS SDK pushed to npm AND an
	// internal Helm chart pushed to an OCI registry is realistic.
	//
	// Each entry carries its declared Version so the LLM can either
	// embed the version in the publish step (npm/cargo/python/helm)
	// or — when the version is git-tag-driven (Go modules) — point
	// that out as a NOTE so the user doesn't expect a `version`
	// field to exist on disk.
	PublishableArtifacts PublishableArtifacts `json:"publishable_artifacts,omitempty"`

	// IntegrationTests captures signals that the repo carries
	// "real" integration tests — the kind that need infrastructure
	// (Postgres, NATS, Redis, …) running before they can pass.
	// Drives `sklp init -only ci` to wire a dedicated test-integration
	// step with `services: [...]` instead of a vanilla `go test ./...`.
	IntegrationTests IntegrationTestFacts `json:"integration_tests,omitempty"`
}

// IntegrationTestFacts surfaces the heuristic evidence that a repo
// carries infra-dependent integration tests. Each slice/field is a
// pure DERIVATION over the file system — no Docker, no network — so
// the result is cheap to compute and deterministic.
type IntegrationTestFacts struct {
	// GoFiles lists Go files matching the *_integration_test.go
	// convention OR carrying a `//go:build integration` build tag.
	// Capped at 20 entries (more would just bloat the prompt; the
	// list is illustrative, not exhaustive).
	GoFiles []string `json:"go_files,omitempty"`

	// HasBuildTag is true when at least one Go file in the repo
	// declares `//go:build integration`. Drives the `-tags=integration`
	// flag on the suggested go test command.
	HasBuildTag bool `json:"has_build_tag,omitempty"`

	// ServiceHints is a sorted list of infra service identifiers
	// (e.g. "postgres", "redis", "nats", "clickhouse", "mongodb",
	// "mysql", "mariadb") whose names appear in the test files OR
	// in a compose file at the root. Used to populate the
	// `services: [...]` list of the integration test step.
	ServiceHints []string `json:"service_hints,omitempty"`
}

// LanguageFacts is everything the scan knows about one language
// inside the repo. A repo can have several LanguageFacts entries
// (e.g. Go + TypeScript in the same monorepo).
type LanguageFacts struct {
	// Lang is the canonical short name: "go", "node", "rust",
	// "python", "java", "ruby", "php", "dotnet", "elixir". Always
	// lowercase, always one word, matches what sklp tools: keys use.
	Lang string `json:"lang"`

	// Modules lists each independent buildable unit found in this
	// language. For Go, one per go.mod. For Node with workspaces, one
	// per workspace package. For Rust, one per Cargo.toml package.
	Modules []ModuleFacts `json:"modules,omitempty"`

	// PackageManager refines the Lang for ecosystems with multiple
	// tools: "pnpm", "yarn", "npm", "bun" for node; "pip", "uv",
	// "poetry", "pdm" for python; "cargo" for rust (always). Empty
	// when the language has only one canonical tool (Go).
	PackageManager string `json:"package_manager,omitempty"`

	// PackageManagerVersion is the pinned PackageManager version when the
	// repo declares one (corepack `packageManager: "pnpm@8.15.0"` or
	// engines.pnpm). Empty when unpinned. Lets the runner config bake an
	// exact pnpm/yarn/bun instead of "latest".
	PackageManagerVersion string `json:"package_manager_version,omitempty"`

	// ToolchainVersion is the language version hint extracted from
	// the canonical pin file (go.mod `go 1.25.0`, .nvmrc, .tool-versions,
	// rust-toolchain.toml, .python-version). Empty when none found.
	// The exact wording is preserved so the LLM can pass it through
	// to sklp tools: without inventing a digit.
	ToolchainVersion string `json:"toolchain_version,omitempty"`

	// HasLockfile reports whether a deterministic lockfile is present
	// (go.sum, pnpm-lock.yaml, Cargo.lock, uv.lock, Gemfile.lock...).
	// Used by the audit-preflight rules: an audit preflight is only
	// emitted when a lockfile exists.
	HasLockfile bool `json:"has_lockfile"`
}

// ModuleFacts is one independent buildable unit in a language.
type ModuleFacts struct {
	// Path is the directory containing the module's manifest, relative
	// to Root with forward slashes. Empty string means "the repo root
	// itself is the module".
	Path string `json:"path"`

	// Name is the module's declared identifier (go.mod `module`,
	// package.json `name`, Cargo.toml [package].name). Empty when the
	// manifest didn't declare one (rare but legal).
	Name string `json:"name,omitempty"`

	// Manifest is the basename of the file we parsed for this module
	// (go.mod, package.json, Cargo.toml, pyproject.toml, …). Lets the
	// LLM grep for it without guessing.
	Manifest string `json:"manifest"`
}

// MakefileFacts captures the bits of the root Makefile that matter for
// generating CI / publish contracts. We do NOT try to evaluate the
// Makefile; we only enumerate target names so the LLM can see which
// commands are "make X" and which it would have to spell out.
type MakefileFacts struct {
	// Path is always "Makefile" (we scan only the root one). Kept as
	// a field rather than implicit so consumers don't have to know
	// the convention.
	Path string `json:"path"`

	// Targets lists every target name extracted from the Makefile,
	// in declaration order with duplicates removed. Phony targets
	// (declared after `.PHONY:`) are included; we don't distinguish
	// them from real targets because the LLM doesn't care.
	Targets []string `json:"targets"`
}

// CIHints points at existing CI configuration files so the LLM can
// optionally mine them for command patterns. The scanner does not
// parse them — listing the paths is enough.
type CIHints struct {
	GitHubActions []string `json:"github_actions,omitempty"` // .github/workflows/*.yml
	GitLabCI      []string `json:"gitlab_ci,omitempty"`      // .gitlab-ci.yml
	CircleCI      []string `json:"circleci,omitempty"`       // .circleci/config.yml
	Drone         []string `json:"drone,omitempty"`          // .drone.yml
}

// APIContracts enumerates the machine-readable API descriptions found
// in the repo, grouped by kind. A repo can have several kinds at
// once: OpenAPI for the public HTTP surface, Protobuf for an internal
// gRPC, AsyncAPI for the event bus — each lists every artifact path
// independently so the LLM emits one CI step per kind.
//
// Paths are relative to the repo root with forward slashes.
type APIContracts struct {
	// OpenAPI matches swagger.{json,yaml,yml}, openapi.{json,yaml,yml},
	// and the conventional api/openapi-spec/*.{json,yaml,yml} layout
	// (kubernetes-style). Filenames are matched case-sensitively to
	// avoid false positives on test fixtures named like "TestOpenAPI.go".
	OpenAPI []string `json:"openapi,omitempty"`

	// AsyncAPI matches asyncapi.{json,yaml,yml} at any depth. The
	// AsyncAPI ecosystem is much smaller than OpenAPI's — typically
	// one file per service — but the breaking-change check is just
	// as valuable.
	AsyncAPI []string `json:"asyncapi,omitempty"`

	// GraphQL matches *.graphql, *.graphqls, and the conventional
	// schema.{graphql,gql} files. We do NOT match *.gql alone because
	// the extension is also used for plain query files (not schemas).
	GraphQL []string `json:"graphql,omitempty"`

	// Protobuf matches *.proto at any depth. A buf.yaml is surfaced
	// separately via Tools.Buf so the LLM can pick `buf breaking`
	// instead of generic protoc commands.
	Protobuf []string `json:"protobuf,omitempty"`

	// Avro matches *.avsc and *.avro schema files. Listed for
	// completeness — the CI derivation is empty for now (Avro
	// breaking-change tooling is fragmented).
	Avro []string `json:"avro,omitempty"`

	// Tools captures companion tooling that, when present, lets the
	// LLM pick a specific breaking-change checker instead of the
	// generic "diff the file against origin/main" fallback.
	Tools APIContractTools `json:"tools,omitempty"`
}

// APIContractTools is the set of API-tooling markers the scanner
// recognises. Each field is a boolean because we only need
// "is the tool wired into this repo?" — the path is irrelevant.
type APIContractTools struct {
	// Buf is true when a buf.yaml or buf.gen.yaml is present at the
	// repo root or under any proto directory. Drives the suggestion
	// to use `buf breaking` for the gRPC contract check.
	Buf bool `json:"buf,omitempty"`

	// Swag is true when a docs.go file imports the swaggo/swag
	// runtime, the Go-side convention for `swag init` projects.
	// Drives the suggestion to emit `make swagger` + a `git
	// diff --exit-code -- docs` regen check.
	Swag bool `json:"swag,omitempty"`

	// GraphQLCodegen is true when a codegen.{yml,yaml,ts,js} file is
	// present alongside the GraphQL schemas. Most TS projects rely on
	// the graphql-codegen toolchain; the marker lets the LLM emit
	// `pnpm graphql-codegen` instead of inventing a command.
	GraphQLCodegen bool `json:"graphql_codegen,omitempty"`
}

// PublishableArtifacts bins every non-Docker shippable thing the scan
// found. Each kind keeps its own slice because the publish command,
// the registry concept, and the version invariants differ:
//
//   - npm: pnpm/npm/yarn publish; registry from publishConfig OR scope.
//   - cargo: cargo publish; registry usually crates.io.
//   - python: uv/poetry/twine publish; registry from pyproject.
//   - helm: helm push to an OCI registry.
//   - go SDK: NO publish command — the module is "published" by
//     pushing a git tag the proxy will serve. We surface the module
//     anyway so the LLM emits a NOTE explaining the tag flow.
type PublishableArtifacts struct {
	NpmPackages    []NpmPackage    `json:"npm_packages,omitempty"`
	CargoCrates    []CargoCrate    `json:"cargo_crates,omitempty"`
	PythonPackages []PythonPackage `json:"python_packages,omitempty"`
	HelmCharts     []HelmChart     `json:"helm_charts,omitempty"`
	GoModules      []GoSDKModule   `json:"go_modules,omitempty"`
}

// NpmPackage describes a publishable package.json. Private packages
// are still included (with Private=true) because the LLM needs to see
// them to NOT emit a publish step for them — silently dropping the
// entry would look like a scanner bug.
type NpmPackage struct {
	// Path is the directory of the package.json, relative to root.
	// "" means the root package.json itself.
	Path string `json:"path"`

	// Name is the `name` field. Required by the npm registry, so
	// usually present; empty means the package.json is malformed and
	// the publish would fail anyway.
	Name string `json:"name,omitempty"`

	// Version is the `version` field at scan time. Empty when the
	// package uses dynamic / lerna-version / nx-release-version
	// schemes. The LLM should treat empty as "version is set by a
	// release tool, do not pin in publish.yaml".
	Version string `json:"version,omitempty"`

	// Private mirrors `"private": true`. Such packages MUST NOT be
	// published; the LLM emits a NOTE and skips the step.
	Private bool `json:"private,omitempty"`

	// Registry is the resolved npm registry URL. Resolution order:
	//   1. publishConfig.registry in the package.json
	//   2. inferred from the scope (e.g. @acme/* → GitHub Packages
	//      when a .npmrc says so; otherwise default npmjs.org)
	//   3. empty when no signal was strong enough
	Registry string `json:"registry,omitempty"`
}

// CargoCrate describes a publishable Cargo package. Workspace
// virtual manifests (root Cargo.toml without [package]) are NOT
// included — only real crates are publishable.
type CargoCrate struct {
	Path string `json:"path"`

	// Name comes from [package].name. Required for a publishable
	// crate; empty when scanning a workspace-root with no [package].
	Name string `json:"name,omitempty"`

	// Version comes from [package].version. The crates.io publish
	// will fail if it doesn't match what's not yet on the registry.
	Version string `json:"version,omitempty"`

	// Publish mirrors [package].publish. False when the manifest
	// explicitly sets publish = false; true otherwise (the cargo
	// default). The LLM skips the step when Publish is false.
	Publish bool `json:"publish"`

	// Registry, when non-empty, names the alternative cargo registry
	// the user pinned via publish = ["my-registry"]. The mapping
	// "my-registry" → URL lives in .cargo/config.toml and the LLM
	// can be asked to look it up there.
	Registry string `json:"registry,omitempty"`
}

// PythonPackage describes a publishable pyproject.toml (PEP 621). We
// also support legacy setup.py / setup.cfg projects but they often
// lack a parseable version; the LLM gets a NOTE in that case.
type PythonPackage struct {
	Path string `json:"path"`

	// Name comes from [project].name (PEP 621) or [tool.poetry].name
	// (legacy poetry). Empty when neither is set.
	Name string `json:"name,omitempty"`

	// Version comes from [project].version or [tool.poetry].version.
	// Empty when the project uses dynamic = ["version"] (the version
	// is computed at build time, usually from a git tag).
	Version string `json:"version,omitempty"`

	// Backend is the PEP 517 build backend, lower-cased:
	// "hatchling", "poetry-core", "setuptools.build_meta", "flit-core",
	// "pdm-backend", "uv-build". Empty for setup.py-only projects.
	// Drives the LLM's choice of publish command.
	Backend string `json:"backend,omitempty"`

	// Registry, when non-empty, is the index URL the project
	// publishes to. Source: [tool.poetry.source] for poetry, or
	// [tool.uv.publish-url] / similar. Empty defaults to PyPI.
	Registry string `json:"registry,omitempty"`
}

// HelmChart describes a Helm chart that this repo could push to an
// OCI registry. Detection is "directory containing Chart.yaml". A
// repo can ship several charts (umbrella + subcharts), each gets
// its own entry.
type HelmChart struct {
	Path string `json:"path"`

	// Name from Chart.yaml. Used as the OCI artifact name.
	Name string `json:"name,omitempty"`

	// Version is the CHART version (Chart.yaml: version). Distinct
	// from AppVersion (the application's version).
	Version string `json:"version,omitempty"`

	// AppVersion is the application version the chart wraps. Often
	// equal to a Docker image tag the chart references.
	AppVersion string `json:"app_version,omitempty"`
}

// GoSDKModule describes a Go module that's likely a SHIPPABLE SDK
// (vs an internal apps/<svc> module). Heuristic: the module path
// starts with a known public host (github.com, gitlab.com, ...) AND
// the directory name suggests "sdk" / "client" / "api". The LLM uses
// this to emit a NOTE about git-tag-based publishing, NOT a publish
// command.
type GoSDKModule struct {
	Path string `json:"path"`

	// Module is the go.mod `module` directive.
	Module string `json:"module"`

	// Public is true when the module path resolves on the public
	// Go proxy (heuristic: starts with github.com / gitlab.com /
	// bitbucket.org / golang.org / k8s.io and not under
	// internal/). Private modules are not surfaced as publishable.
	Public bool `json:"public"`
}
