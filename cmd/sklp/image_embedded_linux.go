//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/internal/plan"
	"github.com/lalternativefabrique/sklp/pipeline"
)

// imageParams holds the parsed `sklp image push` flags.
type imageParams struct {
	dockerfile string
	contextDir string
	image      string
	tag        string
	scan       bool
	secrets    []string // "id=<id>,env=<ENV>" or "id=<id>,src=<file>"
	buildArgs  []string // "NAME=value" ARG overrides
	platform   string
}

// imageCommand declares `sklp image push` on the façade: build an OCI image from
// a Dockerfile and push it to a registry, daemonless (the runc builder + OCI
// push). Flag-driven and self-contained — works on any repo with a Dockerfile.
func imageCommand() cli.Command {
	return cli.Command{
		Name:    "image",
		Aliases: []string{"img"},
		Short:   "Build + push an OCI image daemonless (sklp image push)",
		Sub: []cli.Command{
			{
				Name:  "push",
				Short: "Build a Dockerfile and push it to a registry",
				Flags: []cli.Flag{
					{Name: "dockerfile", Type: cli.StringFlag, StringDefault: "Dockerfile", Usage: "path to the Dockerfile (relative to --context)"},
					{Name: "context", Type: cli.StringFlag, StringDefault: ".", Usage: "Docker build context directory"},
					{Name: "image", Type: cli.StringFlag, Usage: "destination image ref WITHOUT tag, e.g. registry.example.com/acme/core (required)"},
					{Name: "tag", Type: cli.StringFlag, Usage: "image tag (default: auto YYYYMMDD-HHMM-<short-sha>, UTC build time + git HEAD)"},
					{Name: "scan", Type: cli.BoolFlag, Usage: "scan the built image with Trivy (daemonless) and abort the push on a HIGH/CRITICAL fixable finding"},
					{Name: "secret", Type: cli.StringSliceFlag, Usage: "build secret: id=<id>,env=<ENV> or id=<id>,src=<file> (repeatable)"},
					{Name: "build-arg", Type: cli.StringSliceFlag, Usage: "set a Dockerfile ARG: NAME=value (repeatable)"},
					{Name: "platform", Type: cli.StringFlag, Usage: "target platform, linux/amd64 or linux/arm64 (default: the host's)"},
				},
				Run: func(ctx context.Context, args cli.Args) int {
					return runImagePush(imageParams{
						dockerfile: args.String("dockerfile"),
						contextDir: args.String("context"),
						image:      args.String("image"),
						tag:        args.String("tag"),
						scan:       args.Bool("scan"),
						secrets:    args.StringSlice("secret"),
						buildArgs:  args.StringSlice("build-arg"),
						platform:   args.String("platform"),
					})
				},
			},
		},
	}
}

// runImagePush builds the Dockerfile and pushes it via plan.NativeBuild.
func runImagePush(p imageParams) int {
	if p.image == "" {
		fmt.Fprintln(os.Stderr, "sklp image push: --image is required")
		return 2
	}

	ctx := context.Background()

	resolvedTag := p.tag
	if resolvedTag == "" {
		repo, err := gitRepoRoot()
		if err != nil {
			fmt.Fprintln(os.Stderr, "sklp image push: --tag omitted and no git repo found; pass --tag explicitly")
			return 2
		}
		t, err := autoTag(repo)
		if err != nil {
			fmt.Fprintln(os.Stderr, "sklp image push: derive tag:", err)
			return 1
		}
		resolvedTag = t
	}

	// Pin the image config .created so builds stay reproducible. HEAD's commit
	// time is the source: it is stable across rebuilds of the same commit,
	// whereas build time would change the digest on every run. Note this NO
	// LONGER matches the tag's ordering — the tag moved to build time because
	// committer dates run backwards (see pipeline.BuildDateLayout) — and that is
	// fine: Image Updater sorts on the TAG, not on this field. Respect a
	// caller-provided SOURCE_DATE_EPOCH; only derive it when absent.
	if os.Getenv("SOURCE_DATE_EPOCH") == "" {
		if repo, err := gitRepoRoot(); err == nil {
			if epoch, err := git(repo, "show", "-s", "--format=%ct", "HEAD"); err == nil {
				os.Setenv("SOURCE_DATE_EPOCH", epoch)
			}
		}
	}

	dfPath := p.dockerfile
	if !filepath.IsAbs(dfPath) {
		dfPath = filepath.Join(p.contextDir, p.dockerfile)
	}

	secrets, err := parseImageSecrets(p.secrets)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp image push: --secret:", err)
		return 2
	}

	buildArgs, err := parseBuildArgs(p.buildArgs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp image push: --build-arg:", err)
		return 2
	}

	cfg := plan.NativeBuildConfig{
		Dockerfile:        dfPath,
		ContextDir:        p.contextDir,
		Image:             p.image,
		Tag:               resolvedTag,
		Secrets:           secrets,
		BuildArgs:         buildArgs,
		Push:              true,
		ScanEnabled:       p.scan,
		ScanSeverity:      "HIGH,CRITICAL",
		ScanIgnoreUnfixed: true,
		Platform:          p.platform,
	}

	if err := plan.NativeBuild(ctx, cfg, os.Stderr, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "sklp image push:", err)
		return 1
	}
	return 0
}

// parseBuildArgs parses --build-arg NAME=value flags into a map.
func parseBuildArgs(raw []string) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(raw))
	for _, s := range raw {
		k, v, ok := strings.Cut(s, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("expected NAME=value, got %q", s)
		}
		out[k] = v
	}
	return out, nil
}

// parseImageSecrets parses --secret flags in BuildKit format:
// "id=<id>,env=<ENV>" or "id=<id>,src=<file>" → []plan.BuildSecret.
func parseImageSecrets(raw []string) ([]plan.BuildSecret, error) {
	out := make([]plan.BuildSecret, 0, len(raw))
	for _, s := range raw {
		var id, env, src string
		for _, kv := range strings.Split(s, ",") {
			k, v, _ := strings.Cut(kv, "=")
			switch k {
			case "id":
				id = v
			case "env":
				env = v
			case "src":
				src = v
			}
		}
		if id == "" {
			return nil, fmt.Errorf("missing id in %q", s)
		}
		if env == "" && src == "" {
			return nil, fmt.Errorf("secret %q: need env= or src=", id)
		}
		out = append(out, plan.BuildSecret{ID: id, Env: env, Src: src})
	}
	return out, nil
}

// autoTag derives the immutable YYYYMMDD-HHMM-<short-sha> tag for git HEAD in
// repoDir, identical to the publish pipeline's date-sha `publish_tag`.
//
// The date is BUILD time (UTC), not HEAD's committer date — see
// pipeline.BuildDateLayout for why: committer dates run backwards whenever a
// bot commit lands on top of a newer one, minting a tag that sorts below what
// is already deployed so the rollout never happens.
func autoTag(repoDir string) (string, error) {
	full, err := git(repoDir, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolve sha: %w", err)
	}
	return time.Now().UTC().Format(pipeline.BuildDateLayout) + "-" + pipeline.AbbrevSHA(full), nil
}

// gitRepoRoot returns the git repository root containing the current working
// directory, or an error when cwd is not inside a repo.
func gitRepoRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return git(cwd, "rev-parse", "--show-toplevel")
}

// git runs one git command in dir and returns trimmed stdout.
func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// imageCacheRoot is the host cache root for the image builder, matching the
// embedded runtime layout (<XDG_CACHE_HOME|~/.cache>/sklp/embedded-runtime).
func imageCacheRoot() (string, error) {
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve cache home: %w", err)
		}
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, "sklp", "embedded-runtime"), nil
}
