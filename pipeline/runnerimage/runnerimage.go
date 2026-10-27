// Package runnerimage builds the project's runner OCI image from
// .sklp/space.yaml (the closed-environment recipe; legacy name
// config.yaml is still read and auto-migrated, see ResolvePath). The
// recipe describes the environment the project needs to run its
// pipelines in a container — toolchain, packages, Go tools, env, secrets.
//
// Two render modes (see Runner.BaseImage):
//
//   - from-base (runner.base_image set): FROM an image that already
//     ships a compiled sklp (sklp-runner-base) and only layer the
//     project toolchain on top. No repo COPY, so the build context can
//     be empty. This is the mode core's per-project builder uses and the
//     only one that works outside the skalpai repo.
//
//   - from-source (no base_image): compile sklp from the skalpai sources
//     in a builder stage. Requires the skalpai repo as the build
//     context; only used to (re)build skalpai's own runner image during
//     development.
package runnerimage

import (
	"fmt"
	"github.com/lalternativefabrique/sklp/space/host"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// sortedKeys returns a map's keys in deterministic order. Rendering must
// be deterministic: the per-project builder hashes the rendered
// Dockerfile to key the image, so an unstable map iteration order would
// produce a different hash every run and defeat the build cache.
func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// Where skt installs and exposes the project toolchain inside the runner
// image. toolsCache is the content-addressed cache root skt writes into
// (mirrors the runtime SKLP_TOOLS_CACHE); toolsBin is the STABLE symlink
// farm the entrypoint puts on PATH — every installed tool's bin_dir is
// symlinked in here so a single fixed PATH entry exposes the whole
// toolchain regardless of the resolved version subdirectory skt chose.
const (
	toolsCache = "/opt/sklp/tools"
	toolsBin   = toolsCache + "/bin"
)

// sktBinPath is the location the runner image bakes the skt binary at;
// it must match toolchain.bakedSktPath so the runtime resolver finds it.
const sktBinPath = "/usr/local/bin/skt"

// SktBinPath is the in-rootfs path the rendered recipe expects skt at. Exposed
// so the dev builder can bake the host skt there when the base lacks it.
func SktBinPath() string { return sktBinPath }

// staticRunnerPATH is the PATH baked into the runner image. The skt
// symlink farm (toolsBin) comes first so the project toolchain shadows
// any system copy, then the usual system dirs. Known fully at render
// time, so it is a static ENV (no $HOME indirection needed).
const staticRunnerPATH = toolsBin + ":/usr/local/bin:/usr/local/sbin:/usr/bin:/usr/sbin:/bin:/sbin"

// sktInstallSymlink renders the RUN line that installs one tool via skt
// and symlinks every binary skt reports into the stable farm. The JSON
// report is a single compact line (apps/skt reporter), so extracting
// "bin_dir" with sed is safe; jq is not assumed present.
func sktInstallSymlink(b *strings.Builder, spec string) {
	sktInstallSymlinkTags(b, spec, "")
}

// sktInstallSymlinkTags is sktInstallSymlink with optional Go build tags. When
// tags is non-empty the RUN line prefixes GOFLAGS=-tags=<tags>, which `go
// install` (run by skt's go-install backend, which inherits the process env)
// picks up — so a go_tool that needs a build tag to compile a feature in
// (e.g. golang-migrate needs `-tags postgres` to register the postgres driver,
// else `unknown driver postgres` at run time) is built correctly. tags applies
// ONLY to this tool's RUN, not the others.
func sktInstallSymlinkTags(b *strings.Builder, spec, tags string) {
	// GOFLAGS must go INSIDE the command substitution, prefixing `skt` itself —
	// NOT before `out=$(...)`. A `VAR=val out=$(cmd)` line applies VAR to the
	// `out=` assignment, and the assignment does not export it into the $(...)
	// subshell, so `skt` (and its `go install`) would never see it: the build
	// tag is silently dropped and e.g. migrate ships without its postgres driver.
	prefix := ""
	if tags != "" {
		prefix = fmt.Sprintf("GOFLAGS=-tags=%s ", tags)
	}
	fmt.Fprintf(b,
		"RUN out=\"$(%sskt install %s --cache-dir %s --json)\" "+
			"&& bd=\"$(printf '%%s' \"$out\" | sed -n 's/.*\"bin_dir\":\"\\([^\"]*\\)\".*/\\1/p')\" "+
			"&& [ -n \"$bd\" ] && ln -sf \"$bd\"/* %s/\n",
		prefix, spec, toolsCache, toolsBin)
}

// GoToolSktSpec maps a config `go_tools` entry (a full module path with a
// pinned version, e.g. github.com/swaggo/swag/cmd/swag@v1.16.6) to the
// skt spec for the go-install backend: <short-name>@<version>, where the
// short name is the module's last path segment and the leading "v" of the
// version is stripped (skt re-adds it as `go install <module>@v<ver>`).
//
// An entry MAY carry build tags after the version, space-separated, e.g.
// "github.com/golang-migrate/migrate/v4/cmd/migrate@v4.19.1 +tags=postgres".
// The "+tags=" token is stripped from the spec and returned separately so the
// caller can set GOFLAGS for that tool's go install. The third return value is
// those tags ("" when none).
//
// Returns ("", "", false) for a malformed entry (no @version) so the caller
// can surface a clear error instead of baking a broken RUN line.
func GoToolSktSpec(entry string) (spec, tags string, ok bool) {
	mod, tags := splitGoToolTags(entry)
	at := strings.LastIndex(mod, "@")
	if at <= 0 || at == len(mod)-1 {
		return "", "", false
	}
	path, ver := mod[:at], mod[at+1:]
	ver = strings.TrimPrefix(ver, "v")
	name := path[strings.LastIndex(path, "/")+1:]
	if name == "" || ver == "" {
		return "", "", false
	}
	return name + "@" + ver, tags, true
}

// splitGoToolTags splits a go_tools entry into its module@version part and its
// optional build tags. The tags are written as a trailing space-separated
// "+tags=<comma,list>" token; everything before it is the module pin. Absent
// the token, tags is "".
func splitGoToolTags(entry string) (mod, tags string) {
	fields := strings.Fields(entry)
	if len(fields) == 0 {
		return entry, ""
	}
	mod = fields[0]
	for _, f := range fields[1:] {
		if t, found := strings.CutPrefix(f, "+tags="); found {
			tags = t
		}
	}
	return mod, tags
}

type Config struct {
	Runner Runner `yaml:"runner"`
}

type Runner struct {
	// BaseImage, when set, is used as the FROM of the final image. It
	// must already contain a compiled `sklp` (e.g. sklp-runner-base).
	// In this mode the renderer does NOT recompile sklp from source, so
	// the build needs NO repo context — only the toolchain + go_tools
	// layer is added on top. This is the mode core's per-project builder
	// uses, and the only mode that works when building outside the
	// skalpai repo (a user's project, an in-cluster Job, etc.).
	//
	// Empty BaseImage falls back to the legacy "compile sklp from
	// source" mode, which requires the skalpai repo as build context.
	// That mode only makes sense when (re)building skalpai's own runner
	// image during skalpai development.
	BaseImage string            `yaml:"base_image"`
	Toolchain map[string]string `yaml:"toolchain"`
	Packages  []string          `yaml:"packages"`
	GoTools   []string          `yaml:"go_tools"`
	Env       map[string]string `yaml:"env"`
	Secrets   []string          `yaml:"secrets"`

	// Registry the per-project runner image is pushed to / pulled from.
	// User-owned (e.g. ghcr.io/org). Ref: <registry>/sklp-runner/<project>:<hash>.
	Registry string `yaml:"registry"`

	// RegistryAuthSecret: dockerconfigjson Secret name for Registry auth.
	RegistryAuthSecret string `yaml:"registry_auth_secret"`
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &cfg, nil
}

type BuildOptions struct {
	RepoRoot   string
	ConfigPath string
	Tag        string
	Stdout     io.Writer
	Stderr     io.Writer
}

func Build(opts BuildOptions) error {
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}
	if opts.Tag == "" {
		opts.Tag = "sklp-runner:dev"
	}
	cfgPath := opts.ConfigPath
	if cfgPath == "" {
		cfgPath = ResolvePath(opts.RepoRoot)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		return err
	}

	dockerfile, err := renderDockerfile(cfg.Runner)
	if err != nil {
		return fmt.Errorf("render dockerfile: %w", err)
	}

	tmpDir, err := os.MkdirTemp("", "sklp-runnerimage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	dfPath := filepath.Join(tmpDir, "Dockerfile")
	if err := os.WriteFile(dfPath, []byte(dockerfile), 0o644); err != nil {
		return err
	}

	args := []string{"build", "-f", dfPath, "-t", opts.Tag}
	for _, name := range cfg.Runner.Secrets {
		if v := os.Getenv(name); v == "" {
			fmt.Fprintf(opts.Stderr, "[runnerimage] warning: secret %s is empty; build will continue without it\n", name)
			continue
		}
		args = append(args, "--secret", fmt.Sprintf("id=%s,env=%s", strings.ToLower(name), name))
	}
	args = append(args, opts.RepoRoot)

	cmd := exec.Command("docker", args...)
	cmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=1")
	cmd.Stdout = opts.Stdout
	cmd.Stderr = opts.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker build: %w", err)
	}
	return nil
}

func RenderDockerfile(r Runner) (string, error) {
	return renderDockerfile(r)
}

func renderDockerfile(r Runner) (string, error) {
	// Mode selection: a configured base image means "sklp is already
	// baked, just add the project toolchain" — no source, no repo
	// context. This is the path core uses and the only one that builds
	// outside the skalpai repo.
	if r.BaseImage != "" {
		return renderFromBase(r)
	}
	return renderFromSource(r)
}

// renderFromBase produces a single-stage Dockerfile that starts from an
// image already containing a compiled sklp (BaseImage) and layers the
// project's toolchain + go_tools + packages on top. It emits NO `COPY`
// of repo files, so the build context can be empty — which is exactly
// what the in-cluster builder Job feeds it.
func renderFromBase(r Runner) (string, error) {
	var b strings.Builder

	fmt.Fprintf(&b, "FROM %s\n", r.BaseImage)
	writeAptInstall(&b, host.WithPackages(r.Packages))

	// CRITICAL: the toolchain MUST be baked into the exact dirs the
	// runner entrypoint reads at runtime, or the baked tools are
	// invisible and the runner re-installs them (defeating the whole
	// point). sklp resolves its tools cache from SKLP_TOOLS_CACHE; skt
	// installs into it and we symlink every reported bin_dir into the
	// fixed toolsBin farm that PATH points at. No $HOME indirection — the
	// paths are static, known at render time, so build-time install and
	// runtime lookup agree regardless of $HOME. The base image already
	// ships `skt`; this layer only bakes the project toolchain.
	fmt.Fprintf(&b, "ENV SKLP_TOOLS_CACHE=%s\n", toolsCache)
	fmt.Fprintf(&b, "ENV PATH=%s\n", staticRunnerPATH)
	// GOTOOLCHAIN=local: never auto-download a Go toolchain at run time.
	// The baked go is pinned via the project's config; if it doesn't
	// satisfy a go.work/go.mod directive, fail loudly at build time
	// instead of silently fetching another toolchain into the read-only
	// image at run time (which is what broke the first CI run).
	b.WriteString("ENV GOTOOLCHAIN=local\n")
	fmt.Fprintf(&b, "RUN mkdir -p %s\n", toolsBin)

	// Install each toolchain entry via skt, symlinking its binaries into
	// the stable farm on PATH. Deterministic order (sortedKeys) keeps the
	// Dockerfile byte-stable so the per-project builder's content hash is
	// reproducible.
	for _, tool := range sortedKeys(r.Toolchain) {
		sktInstallSymlink(&b, tool+"@"+r.Toolchain[tool])
	}

	// go_tools install via skt's go-install backend (it bootstraps its
	// own pinned go), then symlink the produced binary into the farm.
	for _, mod := range r.GoTools {
		spec, tags, ok := GoToolSktSpec(mod)
		if !ok {
			return "", fmt.Errorf("runner.go_tools entry %q is not a pinned module path (expected <module>@<version>)", mod)
		}
		sktInstallSymlinkTags(&b, spec, tags)
	}

	// Everything under the tools cache was installed root-owned and some
	// binaries land restrictive modes (e.g. pnpm, a single-file Node
	// bundle Node must READ to run). The CI Pod runs as an arbitrary
	// non-root UID, so make the whole tree readable+traversable+
	// executable for all.
	fmt.Fprintf(&b, "RUN chmod -R a+rX %s\n", toolsCache)

	for _, k := range sortedKeys(r.Env) {
		fmt.Fprintf(&b, "ENV %s=%s\n", k, r.Env[k])
	}

	b.WriteString("WORKDIR /workspace\n")
	// ENTRYPOINT/CMD already set by the base (sklp). We leave the base's
	// contract intact.
	return b.String(), nil
}

// renderFromSource is the legacy path: compile sklp from the skalpai
// sources in a builder stage, then assemble the runtime image. It
// REQUIRES the skalpai repo as the Docker build context (COPY apps/sklp,
// go.work, ...) and therefore only works when building from within the
// skalpai repo — i.e. (re)building skalpai's own runner image during
// development. For any other project, set runner.base_image instead.
func renderFromSource(r Runner) (string, error) {
	if r.Toolchain["go"] == "" {
		return "", fmt.Errorf("runner.toolchain.go is required when building from source (used as the sklp-build stage base); set runner.base_image to skip source compilation")
	}

	var b strings.Builder
	goVer := r.Toolchain["go"]

	// skt build stage: compile the standalone Rust toolchain manager from
	// the in-repo crate (apps/skt). Latest stable rust on trixie to match
	// the debian:trixie-slim runtime glibc; the crate's MSRV is 1.81 and
	// the `1-trixie` tag tracks the newest stable (>= MSRV). A pinned
	// `1.83-trixie` does NOT exist on Docker Hub (trixie variants start in
	// the 1.9x range), so the default uses the rolling major tag.
	// Overridable via SKLP_SKT_RUST_IMAGE to pin a specific build.
	// Cargo.toml/Cargo.lock are copied first so the dependency build layer
	// caches independently of source churn.
	rustImage := os.Getenv("SKLP_SKT_RUST_IMAGE")
	if rustImage == "" {
		rustImage = "rust:1-trixie"
	}
	fmt.Fprintf(&b, "FROM %s AS skt-build\n", rustImage)
	b.WriteString("WORKDIR /skt\n")
	b.WriteString("COPY apps/skt/Cargo.toml apps/skt/Cargo.lock ./\n")
	// Prime the dependency cache against a stub main so `cargo build`
	// compiles only the dep graph; the real sources replace the stub next
	// and only the crate itself recompiles on a source change.
	b.WriteString("RUN mkdir src && echo 'fn main() {}' > src/main.rs && cargo build --release --locked && rm -rf src\n")
	b.WriteString("COPY apps/skt/src src\n")
	b.WriteString("COPY apps/skt/recipes recipes\n")
	// Touch main.rs so cargo sees the real source as newer than the stub
	// build artifact and recompiles the crate (not the cached deps).
	b.WriteString("RUN touch src/main.rs && cargo build --release --locked\n")

	fmt.Fprintf(&b, "\nFROM golang:%s-trixie AS sklp-build\n", goVer)
	b.WriteString("RUN apt-get update && apt-get install -y --no-install-recommends gcc g++ libc6-dev && rm -rf /var/lib/apt/lists/*\n")
	for _, k := range sortedKeys(r.Env) {
		fmt.Fprintf(&b, "ENV %s=%s\n", k, r.Env[k])
	}
	b.WriteString("WORKDIR /build\n")
	b.WriteString("COPY go.work go.work.sum ./\n")
	b.WriteString("COPY apps/sklp apps/sklp\n")
	b.WriteString("COPY apps/engine apps/engine\n")
	b.WriteString("COPY apps/core/go.mod apps/core/go.mod\n")
	b.WriteString("COPY apps/core/go.sum apps/core/go.sum\n")
	b.WriteString("COPY apps/admin-api/go.mod apps/admin-api/go.mod\n")
	b.WriteString("COPY apps/admin-api/go.sum apps/admin-api/go.sum\n")
	b.WriteString("WORKDIR /build/apps/sklp\n")

	if hasSecret(r.Secrets, "GITHUB_TOKEN") {
		b.WriteString(`RUN --mount=type=secret,id=github_token sh -c 'if [ -s /run/secrets/github_token ]; then echo "machine github.com login oauth2 password $(cat /run/secrets/github_token)" > /root/.netrc; fi && go mod download && rm -f /root/.netrc'
`)
	} else {
		b.WriteString("RUN go mod download\n")
	}
	// -buildvcs=false: the build context is COPYed into the image, so its
	// .git (when present) is owned by a different UID than the build user.
	// Go's default VCS stamping then runs `git status` and dies with
	// "error obtaining VCS status: exit status 128" (dubious ownership),
	// failing the whole runner-base build. The sklp version is carried by
	// ldflags elsewhere, not by VCS stamping, so disabling it is safe.
	b.WriteString("RUN CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags=\"-s -w\" -o /out/sklp ./cmd/sklp\n")

	if len(r.GoTools) > 0 {
		for _, mod := range r.GoTools {
			fmt.Fprintf(&b, "RUN CGO_ENABLED=0 GOBIN=/out go install %s\n", mod)
		}
	}

	fmt.Fprintf(&b, "\nFROM %s\n", host.BaseImage)
	writeAptInstall(&b, host.WithPackages(r.Packages))

	b.WriteString("RUN git config --system --add safe.directory /workspace\n")

	// skt is the toolchain manager: bake the binary, then install +
	// symlink each toolchain entry into the stable farm on PATH. No
	// shims, no $HOME indirection — toolsBin is a fixed PATH entry and
	// skt resolves partial pins at build time.
	fmt.Fprintf(&b, "COPY --from=skt-build /skt/target/release/skt %s\n", sktBinPath)
	fmt.Fprintf(&b, "ENV SKLP_TOOLS_CACHE=%s\n", toolsCache)
	fmt.Fprintf(&b, "ENV PATH=%s\n", staticRunnerPATH)
	// GOTOOLCHAIN=local: never auto-download a Go toolchain at run time
	// (see renderFromBase for the full rationale).
	b.WriteString("ENV GOTOOLCHAIN=local\n")
	fmt.Fprintf(&b, "RUN mkdir -p %s\n", toolsBin)

	for _, tool := range sortedKeys(r.Toolchain) {
		sktInstallSymlink(&b, tool+"@"+r.Toolchain[tool])
	}
	// go_tools build via skt's go-install backend, then symlink into the
	// farm. The from-source path also installs them into the sklp-build
	// stage's /out above (go install) — kept there so the legacy GOBIN
	// layout is preserved — but the runtime copy now comes from skt to
	// match the from-base contract.
	for _, mod := range r.GoTools {
		spec, tags, ok := GoToolSktSpec(mod)
		if !ok {
			return "", fmt.Errorf("runner.go_tools entry %q is not a pinned module path (expected <module>@<version>)", mod)
		}
		sktInstallSymlinkTags(&b, spec, tags)
	}
	fmt.Fprintf(&b, "RUN chmod -R a+rX %s\n", toolsCache)

	for _, k := range sortedKeys(r.Env) {
		fmt.Fprintf(&b, "ENV %s=%s\n", k, r.Env[k])
	}

	b.WriteString("COPY --from=sklp-build /out/ /usr/local/bin/\n")

	b.WriteString("WORKDIR /workspace\n")
	b.WriteString(`ENTRYPOINT ["sklp"]
`)
	b.WriteString(`CMD ["--help"]
`)

	return b.String(), nil
}

func hasSecret(secrets []string, name string) bool {
	for _, s := range secrets {
		if s == name {
			return true
		}
	}
	return false
}

func writeAptInstall(b *strings.Builder, packages []string) {
	fmt.Fprintf(b, "RUN apt-get update && apt-get install -y --no-install-recommends %s && rm -rf /var/lib/apt/lists/*\n",
		strings.Join(packages, " "))
}
