// Package spawn runs a sklp pipeline inside a Space (daemonless runc).
//
// Instead of executing a pipeline in-process, it runs `sklp run <task>
// -repo /workspace` inside the runner image via the in-process runc +
// Space runtime (apps/sklp/space), so the toolchain comes from the
// image rather than the host. `sklp run <task>` on a laptop is then
// bit-equivalent to what core does for a remote CI run.
package spawn

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
)

// Provider carries the LLM credentials the spawned runner needs. It
// mirrors the SKLP_* env names the sklp CLI already understands, so an
// operator can reproduce the `<bin> run` by hand for debugging.
type Provider struct {
	Kind    string
	Model   string
	BaseURL string
	APIKey  string
}

// Config groups everything OCISpawner.Spawn needs for one run. It is
// the neutral, CLI-local equivalent of core's SpawnConfig — no domain
// types, no agent package.
type Config struct {
	// RunID is used only for logging context. Empty is fine for the CLI.
	RunID string

	// RepoPath is the absolute path of the repository to bind-mount at
	// /workspace inside the container.
	RepoPath string

	// Provider is the resolved LLM credential. An empty APIKey is
	// allowed; the runner degrades gracefully.
	Provider Provider

	// Secrets are already-resolved KEY=value pairs surfaced to the
	// container as environment variables. Empty/nil = none.
	Secrets map[string]string

	// TaskName names the .sklp/tasks/<TaskName>.yaml the container runs.
	// Empty falls back to "ci".
	TaskName string

	// RunnerKind, when set, selects a sibling runner image ("base",
	// "ops", or a full rg.fr-par.scw.cloud/skalpai/* reference) instead
	// of the spawner's default Image.
	RunnerKind string

	// ExtraArgs are appended to the inner `sklp run <task> ...` argv,
	// after the fixed `-repo /workspace -output json`. The CLI forwards
	// execution flags the operator passed on the host (-no-pull, -force,
	// -no-cache, -base-ref, ...) here so Space behaviour matches the
	// in-process path. The inner run detects SKLP_RUNNER_MODE and runs
	// directly in-process, so no recursive spawn happens.
	ExtraArgs []string

	// EnvFile, when set, is parsed and its KEY=value pairs surfaced inside
	// the Space so the pipeline's declared secrets (GITHUB_TOKEN, ...) are
	// available. On a laptop this is the same `.env` the dev stack reads.
	// Only KEY=value lines are honoured; quoting / `export` are not
	// supported. Empty = no file.
	EnvFile string

	// LiveOut receives the container's stderr (human progress) live and
	// also tees stdout (the JSON report). nil discards the live stream.
	LiveOut io.Writer

	// Services are the pod-like sidecars to launch alongside the runner.
	// nil/empty keeps the single-container behaviour. Populated host-side
	// by the caller from the parsed pipeline; see runner.go's Space block.
	Services []Service
}

// Service is the spawn-package's neutral view of a pipeline sidecar. It
// uses only primitive fields so this file compiles WITHOUT the
// embedded_runtime build tag (oci.go is untagged and must not import the
// gated runtime package). The mapping from this shape into
// runtime.Service happens exclusively in oci_embedded_on.go.
type Service struct {
	Name      string
	Image     string
	Run       string
	Command   []string
	Env       map[string]string
	DependsOn []string
	Oneshot   bool
	Ready     *ServiceReady
	Ports     []string
	Volumes   []string
	Network   string
}

// ServiceReady mirrors schema.Ready with primitive fields so the neutral
// Service stays free of the runtime package. Exactly one of HTTP / TCP /
// Log / File is honoured downstream, picked in that order.
type ServiceReady struct {
	HTTP    string
	TCP     string
	Log     string
	File    string
	Timeout string
}

// OCISpawner runs a pipeline inside a Space (daemonless runc).
type OCISpawner struct {
	// Image is the default OCI reference the spawner runs.
	Image string

	// WorkspaceHost is the host-side prefix the Space caller owns. When
	// set, RepoPath is rebased onto it so a containerised caller and the
	// runtime agree on the bind-mount source. Empty = use RepoPath as-is
	// (the common laptop case).
	WorkspaceHost string

	// Bin selects the backend: "space" (default) or its "embedded" alias.
	Bin string
}

func (s *OCISpawner) Name() string { return "oci" }

// Spawn runs the pipeline in a Space via the in-process runc + Space
// runtime (apps/sklp/space) and returns the JSON report the run
// emitted (or whatever it emitted before failing, when err != nil).
// "embedded" is the legacy alias for "space"; both route the same way.
func (s *OCISpawner) Spawn(ctx context.Context, cfg Config) (string, error) {
	return s.spawnEmbedded(ctx, cfg)
}

// hostMountFor rebases repoPath onto WorkspaceHost when set, so a
// containerised caller and the runtime agree on the source path.
// Otherwise repoPath is used verbatim.
func (s *OCISpawner) hostMountFor(repoPath string) (string, error) {
	if !filepath.IsAbs(repoPath) {
		return "", fmt.Errorf("repo path must be absolute: %q", repoPath)
	}
	if s.WorkspaceHost == "" {
		return repoPath, nil
	}
	base := filepath.Base(repoPath)
	if base == "" || base == "/" {
		return repoPath, nil
	}
	return filepath.Join(s.WorkspaceHost, base), nil
}

// resolveImage maps a logical runner kind to the OCI image to run:
//   - ""            → s.Image (the default).
//   - "base"/"ops"  → swap the catalog suffix on s.Image, keeping
//     registry + tag.
//   - a full reference under s.Image's registry namespace → pass through.
//   - anything else → refuse (auditable supply chain).
func (s *OCISpawner) resolveImage(kind string) (string, error) {
	if kind == "" {
		return s.Image, nil
	}
	switch kind {
	case "base", "ops":
		return swapImageName(s.Image, "sklp-runner-"+kind), nil
	}
	namespace := imageNamespace(s.Image)
	if namespace != "" && strings.HasPrefix(kind, namespace) {
		return kind, nil
	}
	return "", fmt.Errorf("runner kind %q is not in the allowlist (base, ops, or a full %s* reference)", kind, namespace)
}

// imageNamespace is the registry + namespace prefix of an OCI reference,
// trailing slash included: "rg.example.com/team/runner:1" → "rg.example.com/team/".
func imageNamespace(ref string) string {
	i := strings.LastIndex(ref, "/")
	if i < 0 {
		return ""
	}
	return ref[:i+1]
}

// swapImageName replaces the image-name segment of an OCI reference
// while keeping the registry prefix and the tag.
func swapImageName(ref, newName string) string {
	name, tag, hasTag := strings.Cut(ref, ":")
	prefix := ""
	if i := strings.LastIndex(name, "/"); i >= 0 {
		prefix = name[:i+1]
	}
	out := prefix + newName
	if hasTag {
		out += ":" + tag
	}
	return out
}

// sortedKeys returns m's keys in lexical order.
func sortedKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// apiKeyEnvNames returns the env var names the runner reads for a given
// provider. Kept in sync with the sklp/main.go resolution table.
func apiKeyEnvNames(provider string) []string {
	switch provider {
	case "anthropic":
		return []string{"ANTHROPIC_API_KEY"}
	case "openai", "openai-compat":
		return []string{"OPENAI_API_KEY"}
	case "ovh":
		return []string{"OVH_KEY", "OVH_API_KEY"}
	case "deepseek":
		return []string{"DEEPSEEK_API_KEY"}
	case "mistral":
		return []string{"MISTRAL_API_KEY"}
	case "scaleway":
		return []string{"SCW_SECRET_KEY"}
	default:
		return []string{strings.ToUpper(provider) + "_API_KEY"}
	}
}

// quoteErr wraps a backend error with the spawner name for clearer logs.
func quoteErr(name string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("spawner(%s): %w", name, err)
}

// DefaultRunnerImage is used when no image is provided via -runner-image
// or SKLP_CI_RUNNER_IMAGE. Mirrors core's defaultRunnerImage.
const DefaultRunnerImage = "rg.fr-par.scw.cloud/skalpai/sklp-runner-base:latest"
