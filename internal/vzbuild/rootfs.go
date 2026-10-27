package vzbuild

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/lalternativefabrique/sklp/space/embed/meta"
	"github.com/lalternativefabrique/sklp/space/host"
)

// The image holds the guest runtime cache too — pulled layers, per-service
// overlays, built runner rootfs — since none of that can live on the virtiofs
// share. It is sparse, so this is a ceiling rather than a cost: the file only
// occupies what has been written to it.
const rootfsSizeBytes = 64 << 30

// guestExtraPackages are what the VM needs beyond a deploy host: qemu-user
// handlers so a publish step can build a foreign platform (an amd64 image on
// an Apple Silicon Mac). update-binfmts records them at install time; the
// agent registers them at boot, since binfmt_misc is not mounted in the chroot.
var guestExtraPackages = []string{"qemu-user-static", "binfmt-support"}

// buildRootfs assembles the guest rootfs from the pinned OCI base, installs
// the tools sklp shells out to, bakes in sklp/skt/the vz agent, and writes an
// ext4 image.
func buildRootfs(ctx context.Context, o Options) error {
	stage := filepath.Join(o.Work, "rootfs")
	layer := filepath.Join(o.Work, "base-layer.tar.gz")

	if err := pullOCILayer(ctx, meta.GuestBase, o.Arch, layer); err != nil {
		return err
	}
	if err := os.RemoveAll(stage); err != nil {
		return err
	}
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "[vz] unpacking %s\n", meta.GuestBase)
	if err := run(ctx, o.Work, "tar", "xzf", layer, "-C", stage); err != nil {
		return err
	}

	// apt needs a resolver inside the chroot.
	if resolv, err := os.ReadFile("/etc/resolv.conf"); err == nil {
		_ = os.WriteFile(filepath.Join(stage, "etc/resolv.conf"), resolv, 0o644)
	}
	packages := host.WithPackages(guestExtraPackages)
	fmt.Fprintf(os.Stderr, "[vz] installing %d packages\n", len(packages))
	apt := "apt-get update -q && apt-get install -yq --no-install-recommends " +
		strings.Join(packages, " ")
	if err := chroot(ctx, stage, "sh", "-c", "export DEBIAN_FRONTEND=noninteractive; "+apt); err != nil {
		return fmt.Errorf("install guest packages: %w", err)
	}

	if err := bakeBinaries(ctx, o, stage); err != nil {
		return err
	}
	if err := writeSubids(stage); err != nil {
		return err
	}

	img := filepath.Join(o.Out, "rootfs.img")
	fmt.Fprintf(os.Stderr, "[vz] writing %s\n", img)
	_ = os.Remove(img)
	if err := run(ctx, o.Work, "truncate", "-s", fmt.Sprint(rootfsSizeBytes), img); err != nil {
		return err
	}
	return run(ctx, o.Work, "mkfs.ext4", "-q", "-d", stage, "-F", img)
}

// bakeBinaries installs sklp, skt and the guest agent. The agent is the
// image's /init: it mounts what the runtime needs and serves the vsock
// protocol the host drives it with.
func bakeBinaries(ctx context.Context, o Options, stage string) error {
	sklpBin, err := resolveBinary(ctx, o.SklpBin, "sklp", "./cmd/sklp", o.Arch, o.Work, true)
	if err != nil {
		return err
	}
	agent, err := resolveBinary(ctx, o.AgentBin, "sklp-vzagent", "./cmd/sklp-vzagent", o.Arch, o.Work, false)
	if err != nil {
		return err
	}
	if err := install(sklpBin, filepath.Join(stage, "usr/local/bin/sklp")); err != nil {
		return err
	}
	if err := install(agent, filepath.Join(stage, "init")); err != nil {
		return err
	}
	if o.SktBin != "" {
		if err := install(o.SktBin, filepath.Join(stage, "usr/local/bin/skt")); err != nil {
			return err
		}
		return nil
	}
	// skt is a separate Rust crate; building it is optional here because a
	// missing skt only breaks pipelines that declare tools:.
	built := filepath.Join(o.Work, "skt")
	if _, err := os.Stat(built); err == nil {
		return install(built, filepath.Join(stage, "usr/local/bin/skt"))
	}
	fmt.Fprintln(os.Stderr, "[vz] skt not provided (-skt-bin) — pipelines declaring tools: will fail in the guest")
	return nil
}

// resolveBinary returns a linux binary for the guest: the caller's if given,
// otherwise one built from the repo.
func resolveBinary(ctx context.Context, given, name, pkg, arch, work string, embedded bool) (string, error) {
	if given != "" {
		return given, nil
	}
	out := filepath.Join(work, name)
	args := []string{"build", "-trimpath", "-ldflags=-s -w", "-o", out}
	if embedded {
		args = append(args, "-tags", "embedded_runtime")
	}
	args = append(args, pkg)
	goBin, err := lookGo()
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, goBin, args...)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	fmt.Fprintf(os.Stderr, "[vz] building %s for linux/%s\n", name, arch)
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("build %s: %w", name, err)
	}
	return out, nil
}

// lookGo finds the Go toolchain. The build runs under sudo, whose reduced
// PATH usually drops /usr/local/go/bin, so the usual install locations are
// tried before giving up.
func lookGo() (string, error) {
	if p, err := exec.LookPath("go"); err == nil {
		return p, nil
	}
	for _, p := range []string{"/usr/local/go/bin/go", "/usr/lib/go/bin/go", "/opt/go/bin/go"} {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("go toolchain not found: pass prebuilt binaries with -sklp-bin/-skt-bin, or put go on PATH (sudo drops /usr/local/go/bin)")
}

// writeSubids gives the guest a subuid/subgid range for every identity sklp
// may run as. Without a range covering the caller, spacenet degrades to a
// space with no name-based DNS instead of failing outright.
func writeSubids(stage string) error { return host.WriteSubids(stage) }

// pullOCILayer downloads the single rootfs layer of an OCI image straight from
// the registry: no daemon, no client, just the distribution API.
func pullOCILayer(ctx context.Context, image, arch, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	repo, tag, _ := strings.Cut(image, ":")
	if !strings.Contains(repo, "/") {
		repo = "library/" + repo
	}
	token, err := registryToken(ctx, repo)
	if err != nil {
		return err
	}
	digest, err := manifestFor(ctx, repo, tag, arch, token)
	if err != nil {
		return err
	}
	layer, err := singleLayer(ctx, repo, digest, token)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "[vz] fetching %s layer\n", image)
	return download(ctx, "https://registry-1.docker.io/v2/"+repo+"/blobs/"+layer, dst, token)
}

func registryToken(ctx context.Context, repo string) (string, error) {
	u := "https://auth.docker.io/token?service=registry.docker.io&scope=repository:" + repo + ":pull"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	return body.Token, nil
}

func manifestFor(ctx context.Context, repo, tag, arch, token string) (string, error) {
	var index struct {
		Manifests []struct {
			Digest   string `json:"digest"`
			Platform struct {
				Architecture string `json:"architecture"`
				OS           string `json:"os"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	if err := registryJSON(ctx, "https://registry-1.docker.io/v2/"+repo+"/manifests/"+tag, token,
		"application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json",
		&index); err != nil {
		return "", err
	}
	for _, m := range index.Manifests {
		if m.Platform.Architecture == arch && m.Platform.OS == "linux" {
			return m.Digest, nil
		}
	}
	return "", fmt.Errorf("no linux/%s manifest for %s:%s", arch, repo, tag)
}

func singleLayer(ctx context.Context, repo, digest, token string) (string, error) {
	var manifest struct {
		Layers []struct {
			Digest string `json:"digest"`
		} `json:"layers"`
	}
	if err := registryJSON(ctx, "https://registry-1.docker.io/v2/"+repo+"/manifests/"+digest, token,
		"application/vnd.oci.image.manifest.v1+json", &manifest); err != nil {
		return "", err
	}
	if len(manifest.Layers) != 1 {
		return "", fmt.Errorf("expected a single-layer base image, got %d layers", len(manifest.Layers))
	}
	return manifest.Layers[0].Digest, nil
}

func registryJSON(ctx context.Context, url, token, accept string, out any) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", accept)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func chroot(ctx context.Context, root string, argv ...string) error {
	// /proc is needed by apt's post-install scripts.
	proc := filepath.Join(root, "proc")
	_ = os.MkdirAll(proc, 0o755)
	_ = exec.CommandContext(ctx, "mount", "--bind", "/proc", proc).Run()
	defer func() { _ = exec.CommandContext(context.WithoutCancel(ctx), "umount", proc).Run() }()

	cmd := exec.CommandContext(ctx, "chroot", append([]string{root}, argv...)...)
	cmd.Stdout, cmd.Stderr = io.Discard, os.Stderr
	return cmd.Run()
}
