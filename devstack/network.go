package devstack

// Bridge networks and auto-port mapping for parallel `sklp dev` /
// `sklp run ci` invocations on the same host.
//
// Background: v1 launched every container with `--network host`. Two
// dev stacks on the same machine collided on every well-known port
// (postgres 5432, nats 4222, ...) — a real blocker for contributors
// working on several worktrees in parallel. v2 introduces a bridge
// network per run + auto-port mapping for any service with an empty
// host port in `ports:` (e.g. `:5432`).
//
// Compatibility: `network: host` stays valid for one release cycle.
// During the migration window the *default* (empty `network:` field)
// also resolves to host so the three in-repo dev.yaml files
// (acme, bakery, skalpai) keep working with their hard-coded
// `tcp: localhost:<port>` probes. A follow-up PR will migrate those
// files and flip the default to bridge.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/lalternativefabrique/sklp/pipeline"
)

// NetworkMode is the resolved network policy for a single service.
// We resolve early (at launch time) and never look at `svc.Network`
// again — the rest of the code paths only ever see Bridge vs Host.
type NetworkMode int

const (
	// NetworkHost = legacy `--network host`. Kept for one release
	// cycle so in-repo dev.yaml files migrate at their own pace.
	NetworkHost NetworkMode = iota

	// NetworkBridge = run-scoped bridge network with auto-port mapping.
	NetworkBridge
)

// DefaultNetworkMode is the policy applied when a service does not
// declare `network:`. Bridge is the default now that the three
// in-repo dev.yaml files (acme, bakery, skalpai) have migrated off
// hard-coded `tcp: localhost:<port>` probes. Explicit `network: host`
// still works for one more release cycle but emits a deprecation
// warning.
const DefaultNetworkMode = NetworkBridge

// resolveNetwork maps the YAML string to the runtime mode and returns
// (mode, warning). warning is non-empty when the user typed an
// explicit `host` (deprecated) or an unknown value (treated as host).
// An implicit empty value resolves to the current default without any
// warning — bridge is the well-trodden path now.
func resolveNetwork(svc pipeline.Service) (NetworkMode, string) {
	switch strings.ToLower(strings.TrimSpace(svc.Network)) {
	case "bridge":
		return NetworkBridge, ""
	case "host":
		return NetworkHost,
			"network: host is deprecated — switch to network: bridge for parallel-run safety"
	case "":
		return DefaultNetworkMode, ""
	default:
		return NetworkHost,
			fmt.Sprintf("unknown network %q; falling back to host (valid: bridge, host)", svc.Network)
	}
}

// runNetwork holds the run-scoped state (bridge network name). `sklp dev` no
// longer reserves host ports (host exposure is the hostname mux, devHostname);
// services are reached by name:port on the bridge.
type runNetwork struct {
	rt   *ociRuntime
	name string // "" when no bridge service is in the run
}

// newRunNetwork builds an empty runNetwork. The bridge is NOT created
// until ensureNetwork is called — we don't want to shell out to the
// runtime for a pipeline that only has shell `run:` services.
func newRunNetwork(rt *ociRuntime, scope, id string) *runNetwork {
	return &runNetwork{
		rt:   rt,
		name: networkName(scope, id),
	}
}

// networkName produces `sklp-<scope>-<short-id>`. We truncate `id` to
// keep the network name under the 64-char docker limit even when the
// caller hands us a long worktree-hash + project id concatenation.
func networkName(scope, id string) string {
	// 12 hex chars is plenty of entropy for collisions across a
	// single host (the worktree-hash space is small enough) without
	// hitting the runtime's name-length limit when prefixed.
	if len(id) > 32 {
		sum := sha256.Sum256([]byte(id))
		id = hex.EncodeToString(sum[:6])
	}
	return "sklp-" + scope + "-" + id
}

// ensureCreated creates the bridge network if it doesn't exist yet.
// `docker network create` returns a non-zero exit code when the name
// is already in use; we swallow that and treat it as success because
// the only consequence of a duplicate name is "we reuse an existing
// network", which is fine for a same-run retry.
func (r *runNetwork) ensureCreated(ctx context.Context) error {
	if r == nil || r.rt == nil || r.name == "" {
		return nil
	}
	cmd := exec.CommandContext(ctx, r.rt.Name, "network", "create", r.name)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// `docker network create` errors when the network already
		// exists ("Error response from daemon: network with name X
		// already exists"). That's the happy path for our use case
		// (same-run retry, hot-reload). Surface only genuine errors
		// — daemon down, permission denied, etc.
		s := string(out)
		if strings.Contains(s, "already exists") {
			return nil
		}
		return fmt.Errorf("%s network create %s: %v: %s",
			r.rt.Name, r.name, err, strings.TrimSpace(s))
	}
	return nil
}

// destroy removes the bridge network. Idempotent: a network that's
// already gone (because no container ever attached, or because
// `--rm` already cleaned everything up and we removed it earlier) is
// reported as success.
func (r *runNetwork) destroy() {
	if r == nil || r.rt == nil || r.name == "" {
		return
	}
	cmd := exec.Command(r.rt.Name, "network", "rm", r.name)
	cmd.Stdout, cmd.Stderr = nil, nil
	_ = cmd.Run()
}

// devHostname is the `*.localhost` name a dev service is published under by the
// host mux: `<svc>.<scope>.localhost`. scope is devScopeID (per-worktree), so two
// worktrees of the same repo expose the same service on distinct names without an
// /etc/hosts edit — `*.localhost` resolves to 127.0.0.1 natively in browsers and
// systemd-resolved. The name is DERIVED, never declared in dev.yaml.
func devHostname(scope, svc string) string {
	return svc + "." + scope + ".localhost"
}

// devScopeID derives a stable, short identifier for the current
// `sklp dev` invocation from the project root path. Different
// worktrees of the same repo produce different IDs (their abs paths
// differ), so two parallel `sklp dev` invocations land on distinct
// bridge networks. Falls back to a constant on hash failure (will
// reuse a network across worktrees — bad, but never crash dev).
//
// stack scopes the id per-stack so two `sklp dev <a>` / `sklp dev <b>` on
// the same worktree don't collide; stack=="" reproduces the legacy
// repo-only id (no folding) for backward compatibility.
func devScopeID(repoAbs, stack string) string {
	if repoAbs == "" {
		repoAbs, _ = os.Getwd()
	}
	key := repoAbs
	if stack != "" {
		key = repoAbs + "\x00" + stack
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:6])
}

// devSpaceName is the embedded-runtime analogue of the legacy bridge network
// name: a stable, per-worktree space the embedded backend scopes its runc
// containers + spacenet bridge to. Keyed by devScopeID so two worktrees of the
// same repo run in distinct spaces and never reap each other's containers.
// spacenet truncates the derived bridge interface name, so the full string is
// only an id — uniqueness comes from the scope hash.
//
// stack scopes the space per-stack; stack=="" keeps today's single-stack name.
func devSpaceName(repoAbs, stack string) string {
	return "sklp-dev-" + devScopeID(repoAbs, stack)
}
