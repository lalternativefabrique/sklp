package runnerimage

import (
	"strings"
	"testing"
)

func TestRenderDockerfile(t *testing.T) {
	r := Runner{
		Toolchain: map[string]string{"go": "1.25.0", "node": "24.16.0"},
		Packages:  []string{"git", "ca-certificates"},
		GoTools:   []string{"github.com/swaggo/swag/cmd/swag@v1.16.6"},
		Env:       map[string]string{"GOPRIVATE": "github.com/example-org"},
		Secrets:   []string{"GITHUB_TOKEN"},
	}
	df, err := renderDockerfile(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		// skt is compiled from the in-repo crate in its own stage.
		"FROM rust:1-trixie AS skt-build",
		"COPY apps/skt/Cargo.toml apps/skt/Cargo.lock ./",
		"cargo build --release --locked",
		"FROM golang:1.25.0-trixie AS sklp-build",
		"FROM debian:trixie-slim",
		// skt binary baked, then used to install the toolchain.
		"COPY --from=skt-build /skt/target/release/skt /usr/local/bin/skt",
		"skt install go@1.25.0 --cache-dir /opt/sklp/tools --json",
		"skt install node@24.16.0 --cache-dir /opt/sklp/tools --json",
		// go_tools install via skt's go-install backend (short name + ver).
		"skt install swag@1.16.6 --cache-dir /opt/sklp/tools --json",
		// stable symlink farm on PATH.
		"ln -sf \"$bd\"/* /opt/sklp/tools/bin/",
		"ENV PATH=/opt/sklp/tools/bin:",
		"ENV SKLP_TOOLS_CACHE=/opt/sklp/tools",
		"git config --system --add safe.directory /workspace",
		"--mount=type=secret,id=github_token",
		`ENTRYPOINT ["sklp"]`,
	} {
		if !strings.Contains(df, want) {
			t.Errorf("missing %q in:\n%s", want, df)
		}
	}
	if strings.Contains(df, "mise") {
		t.Errorf("from-source Dockerfile must not reference mise:\n%s", df)
	}
}

func TestRenderFromBase(t *testing.T) {
	r := Runner{
		BaseImage: "registry.example.com/acme/sklp-runner-base:latest",
		Toolchain: map[string]string{"php": "8.3", "node": "22"},
		Packages:  []string{"imagemagick"},
		GoTools:   []string{"honnef.co/go/tools/cmd/staticcheck@latest"},
		Env:       map[string]string{"APP_ENV": "ci"},
	}
	df, err := renderDockerfile(r)
	if err != nil {
		t.Fatal(err)
	}
	// from-base: FROM the base, layer toolchain — NO source compilation.
	for _, want := range []string{
		"FROM registry.example.com/acme/sklp-runner-base:latest",
		"skt install php@8.3 --cache-dir /opt/sklp/tools --json",
		"skt install node@22 --cache-dir /opt/sklp/tools --json",
		"apt-get install -y --no-install-recommends nftables iproute2 uidmap util-linux git ca-certificates curl xz-utils udhcpc e2fsprogs imagemagick",
		// go_tools route through skt's go-install backend (short name + ver).
		"skt install staticcheck@latest --cache-dir /opt/sklp/tools --json",
		"ENV APP_ENV=ci",
		"WORKDIR /workspace",
		// The toolchain MUST be baked into the dir the runner entrypoint
		// reads at runtime, or the baked tools are invisible and get
		// re-installed. skt installs into SKLP_TOOLS_CACHE and we symlink
		// every reported bin_dir into the stable farm on PATH.
		"SKLP_TOOLS_CACHE=/opt/sklp/tools",
		"ln -sf \"$bd\"/* /opt/sklp/tools/bin/",
		// Stable symlink farm on PATH, no $HOME indirection.
		"ENV PATH=/opt/sklp/tools/bin:",
		// Never auto-download a Go toolchain into the read-only image.
		"ENV GOTOOLCHAIN=local",
	} {
		if !strings.Contains(df, want) {
			t.Errorf("missing %q in:\n%s", want, df)
		}
	}
	// Critically: NO repo COPY, NO sklp recompilation, NO mise.
	for _, forbidden := range []string{
		"COPY go.mod",
		"COPY . .",
		"AS sklp-build",
		"AS skt-build",
		"go build -o /out/sklp",
		"FROM golang:",
		"mise",
		"MISE_",
	} {
		if strings.Contains(df, forbidden) {
			t.Errorf("from-base Dockerfile must not contain %q:\n%s", forbidden, df)
		}
	}
}

func TestRenderFromBase_NoGoRequired(t *testing.T) {
	// A pure-PHP project has no go toolchain. from-base must still render
	// (the legacy from-source path errors without go; base mode must not).
	r := Runner{
		BaseImage: "sklp-runner-base:latest",
		Toolchain: map[string]string{"php": "8.3"},
	}
	if _, err := renderDockerfile(r); err != nil {
		t.Fatalf("from-base should not require go toolchain: %v", err)
	}
}

func TestDumpForRealConfig(t *testing.T) {
	cfg, err := Load("../../../../.sklp/config.yaml")
	if err != nil {
		t.Skipf("config.yaml not found from cwd: %v", err)
	}
	df, err := renderDockerfile(cfg.Runner)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Generated Dockerfile (%d bytes):\n%s", len(df), df)
}

func TestLoadConfig(t *testing.T) {
	cfg, err := Load("../../../../.sklp/config.yaml")
	if err != nil {
		t.Skipf("config.yaml not found from cwd: %v", err)
	}
	if cfg.Runner.Toolchain["go"] == "" {
		t.Errorf("expected go in toolchain, got %+v", cfg.Runner.Toolchain)
	}
}

// TestGoToolSktSpec_Tags covers the optional "+tags=" suffix on a go_tools
// entry, which lets a tool that needs a build tag (e.g. golang-migrate's
// postgres driver) compile correctly.
func TestGoToolSktSpec_Tags(t *testing.T) {
	spec, tags, ok := GoToolSktSpec("github.com/golang-migrate/migrate/v4/cmd/migrate@v4.19.1 +tags=postgres")
	if !ok {
		t.Fatal("expected a valid spec")
	}
	if spec != "migrate@4.19.1" {
		t.Errorf("spec = %q, want migrate@4.19.1", spec)
	}
	if tags != "postgres" {
		t.Errorf("tags = %q, want postgres", tags)
	}
	// No tags suffix → empty tags, spec unchanged.
	spec, tags, ok = GoToolSktSpec("github.com/swaggo/swag/cmd/swag@v1.16.6")
	if !ok || spec != "swag@1.16.6" || tags != "" {
		t.Errorf("plain entry = (%q, %q, %v), want (swag@1.16.6, \"\", true)", spec, tags, ok)
	}
}

// TestRenderDockerfile_GoToolTags asserts a tagged go_tool renders a RUN line
// that sets GOFLAGS=-tags=<tags> for that tool's skt install, so the build tag
// reaches `go install`.
func TestRenderDockerfile_GoToolTags(t *testing.T) {
	df, err := RenderDockerfile(Runner{
		BaseImage: "base:latest",
		Toolchain: map[string]string{"go": "1.25.0"},
		GoTools:   []string{"github.com/golang-migrate/migrate/v4/cmd/migrate@v4.19.1 +tags=postgres"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(df, "GOFLAGS=-tags=postgres ") || !strings.Contains(df, "skt install migrate@4.19.1") {
		t.Errorf("rendered Dockerfile missing tagged migrate install:\n%s", df)
	}
}
