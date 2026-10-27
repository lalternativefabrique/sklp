//go:build linux

package plan

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	parse "github.com/lalternativefabrique/sklp/pipeline/imagebuild"
	"github.com/lalternativefabrique/sklp/space"
	rtembed "github.com/lalternativefabrique/sklp/space/embed"
	"github.com/lalternativefabrique/sklp/space/imagebuild"
	"github.com/lalternativefabrique/sklp/space/ociproc"
	"github.com/lalternativefabrique/sklp/space/platform"
	"github.com/lalternativefabrique/sklp/space/runnerbuild"
)

// init wires the native build implementation into runPublish's indirection.
func init() { nativeBuildFn = runNativeBuild }

// buildPidsLimit caps processes/threads per build step as a fork-bomb backstop.
const buildPidsLimit = 4096

const orphanBuildWorkMinAge = time.Hour

// buildStepRunner picks the step isolation for an application-image build.
// RuncStepRunner runs each RUN in its own runc container (netns egress via
// slirp + the IMDS/RFC1918 blackhole), but that needs newuidmap/newgidmap and
// a delegated subuid range — both host-only. Already being nested (a Space, a
// privileged CI pod) means those are unavailable, so runc create fails with
// the opaque "nsexec: mapping tool not present" before a single RUN executes.
// UnshareStepRunner chroots into the rootfs instead of spawning a container,
// which needs nothing beyond the uid-0 mapping the nested userns already
// grants — the tradeoff is losing the network blackhole for the duration of
// the build (host netns, best-effort egress, same as its other callers).
func buildStepRunner(cacheRoot string, stdout, stderr *os.File) runnerbuild.StepRunner {
	if runnerbuild.NestedUserns() {
		return runnerbuild.UnshareStepRunner{
			Stdout: stdout,
			Stderr: stderr,
		}
	}
	slirpBin, _ := rtembed.ExtractSlirp(cacheRoot)
	return runnerbuild.RuncStepRunner{
		CacheRoot:     cacheRoot,
		SlirpBin:      slirpBin,
		Stdout:        stdout,
		Stderr:        stderr,
		Limits:        ociproc.Limits{PidsMax: buildPidsLimit},
		RequireLimits: InCI(),
	}
}

// BuildSecret is a build-time secret passed to RUN --mount=type=secret.
// Exactly one of Env or Src must be set.
type BuildSecret struct {
	ID  string // secret id, mounted at /run/secrets/<ID>
	Env string // read plaintext from os.Getenv(Env)
	Src string // host file path used directly
}

// NativeBuildConfig holds all parameters for a daemonless OCI build+push.
// Both the pipeline executor (runNativeBuild) and the CLI (sklp image push)
// use this so the two paths share the same logic exactly.
type NativeBuildConfig struct {
	// Dockerfile path (absolute or relative to ContextDir).
	Dockerfile string
	// ContextDir is the build context root.
	ContextDir string
	// Image is the registry reference without tag.
	Image string
	// Tag is the image tag.
	Tag string
	// Secrets lists build secrets resolved before the first RUN.
	Secrets []BuildSecret
	// BuildArgs are --build-arg overrides for Dockerfile ARG values.
	BuildArgs map[string]string
	// Push controls whether the built image is pushed to the registry.
	Push bool
	// ScanEnabled gates the Trivy scan before push.
	ScanEnabled bool
	// ScanSeverity is the comma-joined Trivy --severity filter.
	ScanSeverity string
	// ScanIgnoreUnfixed mirrors Trivy --ignore-unfixed.
	ScanIgnoreUnfixed bool
	// TrivyImage is the pinned Trivy image reference.
	TrivyImage string
	// Platform is the target os/arch ("" = host).
	Platform string
}

// NativeBuild builds a Dockerfile to an OCI layout, optionally scans it with
// Trivy, and optionally pushes it — daemonless, no Docker required. It is the
// single implementation shared by kind:publish pipeline steps and
// `sklp image push`.
func NativeBuild(ctx context.Context, cfg NativeBuildConfig, stdout, stderr io.Writer) error {
	imageRef := cfg.Image + ":" + cfg.Tag

	dfPath := cfg.Dockerfile
	if !filepath.IsAbs(dfPath) {
		dfPath = filepath.Join(cfg.ContextDir, dfPath)
	}
	dfBytes, err := os.ReadFile(dfPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", dfPath, err)
	}
	df, err := parse.Parse(string(dfBytes))
	if err != nil {
		return fmt.Errorf("parse %s: %w", dfPath, err)
	}

	target, err := platform.Parse(cfg.Platform)
	if err != nil {
		return err
	}
	if err := imagebuild.CheckEmulation(target); err != nil {
		return err
	}

	cacheRoot := nativeImageCacheRoot()
	buildCache := filepath.Join(cacheRoot, "buildcache")

	workParent := filepath.Join(cacheRoot, "work")
	gcOrphanWorkDirs(workParent, time.Now())
	if err := os.MkdirAll(workParent, 0o755); err != nil {
		return err
	}
	workRoot, err := os.MkdirTemp(workParent, "build-*")
	if err != nil {
		return err
	}
	defer removeBuildWork(workRoot)

	secrets, cleanupSecrets, err := resolveBuildSecrets(cfg.Secrets, workRoot)
	if err != nil {
		return fmt.Errorf("secrets: %w", err)
	}
	defer cleanupSecrets()

	fmt.Fprintf(stdout, "[sklp oci-build] build %s for %s (%d stage(s))\n", imageRef, target, len(df.Stages))
	var res imagebuild.Result
	err = withFileWriters(stdout, stderr, func(outF, errF *os.File) error {
		var buildErr error
		res, buildErr = imagebuild.Build(ctx, cacheRoot, filepath.Join(workRoot, "stages"), df,
			imagebuild.Options{ContextDir: cfg.ContextDir, Secrets: secrets, BuildArgs: cfg.BuildArgs, Platform: target, CacheDir: buildCache, Progress: stdout},
			buildStepRunner(cacheRoot, outF, errF))
		return buildErr
	})
	if err != nil {
		fmt.Fprintf(stderr, "[sklp oci-build] build failed: %v\n", err)
		return err
	}

	layout := filepath.Join(workRoot, "oci")
	img, err := imagebuild.ExportImage(ctx, res, layout)
	if err != nil {
		fmt.Fprintf(stderr, "[sklp oci-build] export: %v\n", err)
		return err
	}
	fmt.Fprintf(stdout, "[sklp oci-build] built %s manifest %s\n", imageRef, img.Manifest.Digest)

	if cfg.ScanEnabled {
		if err := scanBuiltRootfs(ctx, cacheRoot, res.Rootfs, cfg, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "[sklp oci-build] scan failed — push aborted: %v\n", err)
			return err
		}
	}

	if !cfg.Push {
		runtimeCache, rerr := space.RuntimeCacheRoot()
		if rerr != nil {
			return rerr
		}
		if err := space.InstallLocalImage(runtimeCache, imageRef, img.Manifest.Digest.String(), img.ConfigJSON, res.Rootfs); err != nil {
			fmt.Fprintf(stderr, "[sklp oci-build] install locally: %v\n", err)
			return err
		}
		fmt.Fprintf(stdout, "[sklp oci-build] installed %s locally (no push)\n", imageRef)
	}

	if cfg.Push {
		fmt.Fprintf(stdout, "[sklp oci-build] push %s\n", imageRef)
		if err := space.PushOCILayout(ctx, layout, img.Manifest, imageRef); err != nil {
			if looksLikeRegistryAuthFailure(err) {
				fmt.Fprintf(stderr, "[sklp oci-build] push failed: %s\n",
					describePushAuthFailure(imageHostOf(cfg.Image), RegistryLoginWasWritten(), err))
				return fmt.Errorf("push %s: %s", imageRef,
					pushAuthHint(imageHostOf(cfg.Image), RegistryLoginWasWritten()))
			}
			fmt.Fprintf(stderr, "[sklp oci-build] push failed: %v\n", err)
			return err
		}
		fmt.Fprintf(stdout, "[sklp oci-build] pushed %s@%s\n", imageRef, img.Manifest.Digest)
	}

	fmt.Fprintf(stdout, "[sklp oci-build] ok %s\n", imageRef)
	return nil
}

// runNativeBuild executes a `kind: publish` step via NativeBuild.
func runNativeBuild(ctx context.Context, cfg CIExecConfig, step CIStep, _ string, stdoutW, stderrW io.Writer, _ publishExec) (int, error) {
	if err := NativeBuild(ctx, nativeBuildConfig(cfg.Repo, step.Publish), stdoutW, stderrW); err != nil {
		return 1, err
	}
	return 0, nil
}

func nativeBuildConfig(repo string, spec *PublishSpec) NativeBuildConfig {
	secrets := make([]BuildSecret, 0, len(spec.BuildSecrets))
	for _, s := range spec.BuildSecrets {
		secrets = append(secrets, BuildSecret{ID: s.ID, Env: s.Env, Src: s.File})
	}
	return NativeBuildConfig{
		Dockerfile:        filepath.Join(repo, spec.Dockerfile),
		ContextDir:        filepath.Join(repo, spec.Context),
		Image:             spec.Image,
		Tag:               spec.Tag,
		Secrets:           secrets,
		BuildArgs:         spec.BuildArgs,
		Push:              spec.Push,
		ScanEnabled:       spec.ScanEnabled,
		ScanSeverity:      spec.ScanSeverity,
		ScanIgnoreUnfixed: spec.ScanIgnoreUnfixed,
		TrivyImage:        spec.TrivyImage,
		Platform:          spec.Platform,
	}
}

// resolveNativeSecrets turns the step's build secrets into id->host-file paths
// the builder bind-mounts. An env-sourced secret is materialised into a private
// temp file under secretDir; a file-sourced secret is used in place. Returns a
// cleanup for any temp files written.
func ResolveNativeSecrets(secrets []PublishSecret, secretDir string) (map[string]string, func(), error) {
	return resolveNativeSecrets(secrets, secretDir)
}

func resolveNativeSecrets(secrets []PublishSecret, secretDir string) (map[string]string, func(), error) {
	out := map[string]string{}
	var tmp []string
	cleanup := func() {
		for _, p := range tmp {
			_ = os.Remove(p)
		}
	}
	for _, s := range secrets {
		switch {
		case s.File != "":
			out[s.ID] = expandTilde(s.File)
		case s.Env != "":
			v, ok := os.LookupEnv(s.Env)
			if !ok {
				cleanup()
				return nil, nil, fmt.Errorf("build secret %q: env %s not set", s.ID, s.Env)
			}
			f, err := os.CreateTemp(secretDir, "secret-"+s.ID+"-*")
			if err != nil {
				cleanup()
				return nil, nil, err
			}
			if _, err := f.WriteString(v); err != nil {
				f.Close()
				cleanup()
				return nil, nil, err
			}
			f.Close()
			tmp = append(tmp, f.Name())
			out[s.ID] = f.Name()
		}
	}
	return out, cleanup, nil
}

// resolveBuildSecrets materialises []BuildSecret into the id→hostFile map that
// imagebuild.Options.Secrets expects. Env-sourced secrets are written to tmp
// files under secretDir; Src-sourced secrets are used in place.
func resolveBuildSecrets(secrets []BuildSecret, secretDir string) (map[string]string, func(), error) {
	out := map[string]string{}
	var tmp []string
	cleanup := func() {
		for _, p := range tmp {
			_ = os.Remove(p)
		}
	}
	for _, s := range secrets {
		switch {
		case s.Src != "":
			out[s.ID] = expandTilde(s.Src)
		case s.Env != "":
			v, ok := os.LookupEnv(s.Env)
			if !ok {
				cleanup()
				return nil, nil, fmt.Errorf("build secret %q: env %s not set", s.ID, s.Env)
			}
			f, err := os.CreateTemp(secretDir, "secret-"+s.ID+"-*")
			if err != nil {
				cleanup()
				return nil, nil, err
			}
			if _, err := f.WriteString(v); err != nil {
				f.Close()
				cleanup()
				return nil, nil, err
			}
			f.Close()
			tmp = append(tmp, f.Name())
			out[s.ID] = f.Name()
		}
	}
	return out, cleanup, nil
}

// scanBuiltRootfs gates the push on a Trivy scan of the freshly-built image
// filesystem. Trivy runs from the pinned InternalTrivyImage pulled as a rootfs
// via the embedded puller, NOT from the host PATH: a host-resolved binary makes
// the scan verdict depend on whichever Trivy version the machine happens to
// carry, so the same commit can pass on one host and fail on another.
func scanBuiltRootfs(ctx context.Context, cacheRoot, target string, cfg NativeBuildConfig, stdout, stderr io.Writer) error {
	trivyImg := cfg.TrivyImage
	if trivyImg == "" {
		trivyImg = InternalTrivyImage
	}

	absTarget, err := filepath.Abs(target)
	if err != nil {
		return err
	}

	trivyRootfs, err := space.PrepareImage(ctx, cacheRoot, trivyImg)
	if err != nil {
		return fmt.Errorf("pull scanner image %s: %w", trivyImg, err)
	}

	fmt.Fprintf(stdout, "[sklp oci-build] scan (%s, severity=%s)\n", trivyImg, cfg.ScanSeverity)
	return withFileWriters(stdout, stderr, func(outF, errF *os.File) error {
		runner := runnerbuild.RuncStepRunner{
			CacheRoot: cacheRoot,
			Stdout:    outF,
			Stderr:    errF,
			CacheMounts: []runnerbuild.CacheMount{
				{Source: absTarget, Target: "/scan"},
			},
		}
		return runner.Exec(ctx, trivyRootfs, []string{trivyRootfsCommand(cfg.ScanSeverity, cfg.ScanIgnoreUnfixed, "/scan")})
	})
}

// trivyRootfsCommand builds the in-rootfs Trivy invocation. Split out so a test
// can assert the flags without running a container.
func trivyRootfsCommand(severity string, ignoreUnfixed bool, target string) string {
	parts := []string{
		"trivy", "rootfs",
		"--severity", shellQuote(severity),
		"--exit-code", "1",
		"--no-progress",
		"--scanners", "vuln",
	}
	if ignoreUnfixed {
		parts = append(parts, "--ignore-unfixed")
	}
	return strings.Join(append(parts, target), " ")
}

// nativeImageCacheRoot is the persistent host cache for pulled bases + the build
// cache, shared across native builds on this host.
// NativeImageCacheRoot is the exported variant used by `sklp clean`.
func NativeImageCacheRoot() string { return nativeImageCacheRoot() }

func nativeImageCacheRoot() string {
	if v := os.Getenv("XDG_CACHE_HOME"); v != "" {
		return filepath.Join(v, "sklp", "imagebuild")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".cache", "sklp", "imagebuild")
	}
	return filepath.Join(os.TempDir(), "sklp-imagebuild")
}

// removeBuildWork deletes a build work dir. Stage rootfs contain files chowned
// to mapped subuids (from the unshare RUNs), which the host uid cannot unlink
// directly, so the removal runs in the same userns mapping; a plain RemoveAll is
// the fallback. Best-effort.
func removeBuildWork(dir string) {
	cmd := exec.Command("unshare",
		"--user", "--map-root-user",
		"--map-users=1:100000:65535", "--map-groups=1:100000:65535",
		"--mount", "rm", "-rf", dir)
	if err := cmd.Run(); err != nil {
		_ = os.RemoveAll(dir)
	}
}

var removeBuildWorkFn = removeBuildWork

// gcOrphanWorkDirs removes leftover build-* work dirs from previous runs that
// were interrupted (Ctrl-C / SIGKILL) before their defer cleanup ran — so the
// multi-gigabyte stage rootfs do not accumulate on disk. Best-effort.
func gcOrphanWorkDirs(workParent string, now time.Time) {
	entries, err := os.ReadDir(workParent)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "build-") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) >= orphanBuildWorkMinAge {
			removeBuildWorkFn(filepath.Join(workParent, e.Name()))
		}
	}
}

// fileWriter returns w as an *os.File: itself when it already is one, else the
// write end of a pipe drained into w. wait closes the write end and blocks until
// every byte reached w; call it once the child process has exited.
func fileWriter(w io.Writer) (f *os.File, wait func() error, err error) {
	if f, ok := w.(*os.File); ok {
		return f, func() error { return nil }, nil
	}
	r, pw, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	done := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(w, r)
		r.Close()
		done <- copyErr
	}()
	return pw, func() error {
		closeErr := pw.Close()
		if copyErr := <-done; copyErr != nil {
			return copyErr
		}
		return closeErr
	}, nil
}

func withFileWriters(stdout, stderr io.Writer, fn func(outF, errF *os.File) error) error {
	outF, waitOut, err := fileWriter(stdout)
	if err != nil {
		return err
	}
	errF, waitErr, err := fileWriter(stderr)
	if err != nil {
		waitOut()
		return err
	}
	runErr := fn(outF, errF)
	outWaitErr := waitOut()
	errWaitErr := waitErr()
	if runErr != nil {
		return runErr
	}
	if outWaitErr != nil {
		return outWaitErr
	}
	return errWaitErr
}

// nativeScanSpec builds the sandboxSpec for the Trivy scan of the OCI layout:
// network on (Trivy fetches its vuln DB), the layout dir granted read access via
// the secret-dir-style grant, step env + secrets as usual.
func nativeScanSpec(cfg CIExecConfig, step CIStep, cmd, layoutDir string, out, errW io.Writer) sandboxSpec {
	// Trivy runs inside a userns (--map-root-user) where it cannot read the
	// caller's ~/.docker/config.json (mode 600, owned by the real uid). Point
	// DOCKER_CONFIG at an empty temp dir so Trivy skips credential lookup
	// entirely — the OCI layout is local, no registry auth needed for scanning.
	env := make(map[string]string, len(step.Env)+1)
	for k, v := range step.Env {
		env[k] = v
	}
	if _, ok := env["DOCKER_CONFIG"]; !ok {
		if tmp, err := os.MkdirTemp("", "sklp-trivy-docker-*"); err == nil {
			env["DOCKER_CONFIG"] = tmp
		}
	}
	return sandboxSpec{
		Repo:      cfg.Repo,
		Cwd:       step.Cwd,
		Cmdline:   cmd,
		StepEnv:   env,
		HostEnv:   cfg.BaseEnv,
		Secrets:   step.Secrets,
		AllSecret: cfg.Secrets,
		Network:   "", // Trivy needs network for its DB
		SecretDir: layoutDir,
		Stdout:    out,
		Stderr:    errW,
		Timeout:   cfg.PerStepTimeout,
	}
}

// expandTilde expands a leading ~ to the user's home (file-sourced secrets).
func expandTilde(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}
