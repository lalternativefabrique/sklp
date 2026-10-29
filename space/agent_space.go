package space

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// AgentConfig describes the space an AI agent's commands run in. It is the CI
// runner's environment — same recipe, same builder, same tool caches — opened
// for a whole session instead of one pipeline, and without anything the CI
// hands its runner that an agent must not hold: no secrets passphrase, no
// readable secret file, no write to what runs on its own later.
type AgentConfig struct {
	// RepoPath is the workspace, mounted read-write at /workspace.
	RepoPath string
	// Hide lists extra host files under RepoPath to mount empty, beyond the
	// secret files found by name.
	Hide []string
	// Mounts are extra binds, such as the host's sklp for `sklp run ci`.
	Mounts []DevMount
	// Out receives the rootfs build log.
	Out io.Writer
}

// AgentWorkspace is where the repository is mounted in an agent space.
const AgentWorkspace = "/workspace"

// AgentSpaceName is the space a repository's agent gets: stable per
// checkout, so a restarted agent reopens the same one and its caches.
func AgentSpaceName(repoPath string) string {
	sum := sha256.Sum256([]byte(repoPath))
	return "agent-" + sanitizeSpaceName(strings.ToLower(filepath.Base(repoPath))) + "-" + hex.EncodeToString(sum[:])[:8]
}

// agentReadOnly are workspace paths an agent reads but never writes: its own
// permission rules and what git or an editor runs on the person's behalf.
var agentReadOnly = []string{".git/hooks", ".git/config", ".ai", ".claude", ".codex", ".sklp"}

// RunAgentSpace opens the agent space for cfg.RepoPath and holds it until ctx
// ends, then tears it down. Commands enter it with `sklp space exec <name>`.
func RunAgentSpace(ctx context.Context, cfg AgentConfig) error {
	repo, err := filepath.Abs(cfg.RepoPath)
	if err != nil {
		return err
	}
	name := AgentSpaceName(repo)
	sp, err := NewSpace(name)
	if err != nil {
		return fmt.Errorf("agent space: %w", err)
	}
	if err := requireSpaceHost(sp.CacheRoot(), cfg.Out); err != nil {
		return err
	}
	defer sp.Close()

	buildOut, closeBuildOut := liveOutFile(cfg.Out)
	rootfs, bakedEnv, _, unlock, err := buildSpaceRootfs(ctx, sp, repo, "", buildOut)
	closeBuildOut()
	defer unlock()
	if err != nil {
		return fmt.Errorf("agent space: build rootfs: %w", err)
	}

	// The module cache is the CI's own, content-addressed and safe to share;
	// HOME and the build cache are this agent's, like a CI task's bucket.
	sharedCache := filepath.Join(sp.CacheRoot(), "ci-toolcache", "shared")
	agentCache := filepath.Join(sp.CacheRoot(), "agent-toolcache", name)
	for _, dir := range []string{filepath.Join(sharedCache, "go/pkg/mod"), filepath.Join(agentCache, "go-tmp"), filepath.Join(agentCache, ".cache/go-build")} {
		if err := ensurePrivateDir(dir); err != nil {
			return fmt.Errorf("agent space: cache dir: %w", err)
		}
	}

	mounts := []DevMount{
		{Source: repo, Destination: AgentWorkspace},
		{Source: sharedCache, Destination: "/cache-shared"},
		{Source: agentCache, Destination: "/cache"},
	}
	if main := mainRepoForWorktree(repo); main != "" {
		mounts = append(mounts, DevMount{Source: main, Destination: main, ReadOnly: true})
	}
	for _, rel := range agentReadOnly {
		if _, err := os.Stat(filepath.Join(repo, rel)); err == nil {
			mounts = append(mounts, DevMount{Source: filepath.Join(repo, rel), Destination: filepath.Join(AgentWorkspace, rel), ReadOnly: true})
		}
	}
	for _, path := range append(agentSecretFiles(repo), cfg.Hide...) {
		rel, err := filepath.Rel(repo, path)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		mounts = append(mounts, DevMount{Source: os.DevNull, Destination: filepath.Join(AgentWorkspace, rel), ReadOnly: true})
	}
	mounts = append(mounts, cfg.Mounts...)

	// The recipe's env (PATH to the toolchain) and the cache redirects, and
	// nothing from the host: no passphrase, unlike the CI runner.
	env := append(append([]string{}, bakedEnv...), writableCacheEnv()...)
	agent := DeployService{
		Name:             "agent",
		RootfsDir:        rootfs,
		RootfsCfg:        ImageConfig{Env: bakedEnv},
		IgnoreEntrypoint: true,
		PauseInit:        true,
		Env:              env,
		WorkdirOverride:  AgentWorkspace,
		DevMounts:        mounts,
	}
	if _, err := sp.Start(ctx, agent); err != nil {
		return fmt.Errorf("agent space: start: %w", err)
	}
	<-ctx.Done()
	return nil
}

// agentSecretFiles finds the files of repo that hold secrets by their name.
func agentSecretFiles(repo string) []string {
	var found []string
	_ = filepath.WalkDir(repo, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "target", "dist":
				if path != repo {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if d.Type().IsRegular() && isAgentSecretFile(d.Name()) {
			found = append(found, path)
		}
		return nil
	})
	return found
}

func isAgentSecretFile(name string) bool {
	switch {
	case name == ".env", strings.HasPrefix(name, ".env.") && !strings.HasSuffix(name, ".example") && !strings.HasSuffix(name, ".sample") && !strings.HasSuffix(name, ".template"):
		return true
	case name == ".netrc", name == ".pgpass", name == ".npmrc", name == ".pypirc", name == ".git-credentials":
		return true
	case strings.HasSuffix(name, ".pem"), strings.HasSuffix(name, ".key"), strings.HasSuffix(name, ".p12"), strings.HasSuffix(name, ".pfx"):
		return true
	case strings.HasPrefix(name, "id_rsa"), strings.HasPrefix(name, "id_ed25519"):
		return !strings.HasSuffix(name, ".pub")
	}
	return false
}
