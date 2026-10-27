// deploy_remote_deploy.go routes a PLAIN `sklp deploy [stack]` to the right
// transport, kubectl-style: the ACTIVE context (config `current_context`, or
// --context) decides. A context with a Server reconciles the manifest against
// that remote supervisor over the network (deployer.RemoteApply — the manifest is
// read LOCALLY and POSTed, nothing is copied to the server); an explicit --local,
// or no server resolved at all, falls through to the local socket flow.
//
// This is the deploy-verb counterpart of deploy_remote_net.go (which handles the
// read/control sub-verbs). Non-gated: the manifest resolution + network client
// need no embedded runtime; only the SERVER does.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/lalternativefabrique/sklp/internal/config"
	"github.com/lalternativefabrique/sklp/internal/layout"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
	"github.com/lalternativefabrique/sklp/spacemanager/deployer"
)

// maybeRunRemoteDeploy inspects a plain `sklp deploy` invocation (no read/control
// sub-verb) and, when the active context is remote, runs the deploy as a network
// apply and returns handled=true. It returns handled=false when the invocation is
// a sub-verb (handled elsewhere), when --local forces the socket, or when the
// resolved context has no Server — leaving the local flow untouched.
func maybeRunRemoteDeploy(args []string) (handled bool, exitCode int) {
	// A leading non-flag token is either a stack name (plain deploy) or a sub-verb.
	// Sub-verbs are handled by maybeRunRemoteNet / the gated tree; skip them here.
	if verb := leadingSubverb(args); isDeploySubverb(verb) {
		return false, 0
	}
	// `-h`/`--help`: let the façade render the sub-verb catalogue, don't shim.
	if wantsHelp(args) {
		return false, 0
	}

	dp, err := parseDeployTransport(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp deploy:", err)
		return true, 2
	}
	if dp.local {
		return false, 0 // explicit --local: force the socket flow
	}
	if dp.server == "" {
		return false, 0 // context is local (no Server): normal local flow
	}
	if dp.stack == "" {
		// Remote context but no stack: this is a discovery call, not a deploy.
		// Fall through so the façade lists the deploy sub-verbs instead of erroring.
		return false, 0
	}
	if !dp.explicitTarget {
		fmt.Fprint(os.Stderr, implicitTargetRefusal(dp))
		return true, 2
	}

	return true, runRemoteDeploy(dp)
}

// isDeploySubverb reports whether tok is one of the deploy sub-verbs that own
// their own transport handling (so the plain-deploy router must not claim them).
func isDeploySubverb(tok string) bool { return deploySubverbs[tok] }

// deployTransport is the resolved plain-deploy invocation: which stack/space to
// apply and where (server + auth), after folding in the active context.
type deployTransport struct {
	stack    string
	space    string
	repo     string
	local    bool // --local: force the local socket even if the context is remote
	force    bool // --force: adopt a space owned by a DIFFERENT stack (server-side guard)
	server   string
	token    string
	caFinger string
	insecure bool
	context  string
	// explicitTarget is false when the node came only from current_context:
	// an apply must never land on whatever context was last selected.
	explicitTarget bool
}

func implicitTargetRefusal(dp deployTransport) string {
	return fmt.Sprintf("sklp deploy: refusing to apply %s to the active context %q (%s) implicitly.\n"+
		"Name the target: sklp deploy apply %s -context <name>  (or SKLP_CONTEXT=<name>, or -server <url>).\n"+
		"Contexts: sklp deploy context list\n", dp.stack, dp.context, dp.server, dp.stack)
}

// parseDeployTransport scans a plain `sklp deploy` arg tail for the stack
// positional and the transport-relevant flags (--space/--repo/--local + the
// network flags), then folds in the active context's server/token/fingerprint/
// space so a bare `sklp deploy` follows `current`. Explicit flags always win.
func parseDeployTransport(args []string) (deployTransport, error) {
	dp := deployTransport{repo: "."}
	var contextName, spaceFlag string
	spaceSet := false
	serverFlag := false

	for i := 0; i < len(args); i++ {
		a := args[i]
		name, inline, hasInline := splitFlag(a)
		if name == "" { // a bare token: the stack positional (first one wins)
			if dp.stack == "" {
				dp.stack = a
			}
			continue
		}
		take := func() (string, bool) {
			if hasInline {
				return inline, true
			}
			if i+1 < len(args) {
				i++
				return args[i], true
			}
			return "", false
		}
		switch name {
		case "local":
			dp.local = true
		case "force":
			dp.force = true
		case "space":
			spaceFlag, _ = take()
			spaceSet = true
		case "repo":
			dp.repo, _ = take()
		case "context":
			contextName, _ = take()
		case "config":
			if v, ok := take(); ok && v != "" {
				_ = os.Setenv("SKLP_CONFIG", v) // one file per node/VM
			}
		case "server", "addr":
			dp.server, _ = take()
			serverFlag = dp.server != ""
		case "token":
			dp.token, _ = take()
		case "ca-fingerprint":
			dp.caFinger, _ = take()
		case "insecure":
			dp.insecure = true
		case "token-file":
			v, _ := take()
			if v != "" {
				data, rerr := os.ReadFile(v)
				if rerr != nil {
					return dp, fmt.Errorf("read --token-file: %w", rerr)
				}
				dp.token = strings.TrimSpace(string(data))
			}
		}
	}

	// Fold in the active (or --context) deploy context for anything not set
	// explicitly. An unresolved context leaves dp.server empty, so the caller
	// falls through to the local flow only because no remote was ever named —
	// never because a stale selector pointed at nothing.
	if cfg, _, err := config.Load(); err == nil {
		if name, c, rerr := cfg.ResolveContext(contextName); rerr == nil {
			dp.context = name
			if dp.server == "" {
				dp.server = c.Server
			}
			if dp.caFinger == "" {
				dp.caFinger = c.CAFingerprint
			}
			if dp.token == "" {
				// Context bearer, not the SaaS PAT — see EffectiveContextToken.
				dp.token = config.EffectiveContextToken(c)
			}
			if !spaceSet && c.Space != "" {
				spaceFlag = c.Space
				spaceSet = true
			}
		}
	}

	dp.explicitTarget = contextName != "" || os.Getenv(config.EnvContext) != "" || serverFlag
	dp.space = spaceFlag
	if !spaceSet {
		dp.space = "production" // matches the deploy verb's --space default
	}
	return dp, nil
}

// runRemoteDeploy resolves the local deploy.yaml for the stack and applies it to
// the remote supervisor named by the active context. It returns the process exit
// code.
func runRemoteDeploy(dp deployTransport) int {
	if dp.stack == "" {
		fmt.Fprintln(os.Stderr, "sklp deploy: a stack is required for a remote deploy (e.g. `sklp deploy core`)")
		return 2
	}
	if dp.token == "" {
		fmt.Fprintln(os.Stderr, "sklp deploy: the active context is remote but has no token — run `sklp deploy login` or pass --token")
		return 1
	}
	if dp.caFinger == "" && !dp.insecure {
		fmt.Fprintln(os.Stderr, "sklp deploy: remote deploy needs --ca-fingerprint (from `sklp deploy login`) or --insecure (LAN only)")
		return 1
	}

	path, err := layout.New(dp.repo).Resolve(dp.stack, "deploy")
	if err != nil {
		fmt.Fprintf(os.Stderr, "sklp deploy: %v\n", err)
		return 2
	}
	fmt.Fprintf(os.Stderr, "sklp deploy: applying %s to %s (space %s)\n", dp.stack, describeTarget(dp), dp.space)
	// Must run BEFORE spacemanager.Load (inside RemoteApply) resolves ${VAR}:
	// this sets the process env a project_id: secret needs to be visible for.
	loadProjectSecretsIntoEnv(path)

	var opts []control.DialOption
	if dp.caFinger != "" {
		opts = append(opts, control.WithCAFingerprint(dp.caFinger))
	}
	if dp.insecure {
		opts = append(opts, control.WithInsecure())
	}
	client, err := control.DialNet(dp.server, dp.token, opts...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sklp deploy:", err)
		return 1
	}
	client = client.WithSpace(dp.space)

	err = deployer.RemoteApply(context.Background(), client, deployer.RemoteApplyConfig{
		Stack: dp.stack,
		File:  path,
		Space: dp.space,
		Repo:  dp.repo,
		Force: dp.force,
	}, os.Stdout)
	if err != nil {
		reportNetErr("sklp deploy", dp.space, err)
		return 1
	}
	return 0
}

func describeTarget(dp deployTransport) string {
	if dp.context != "" {
		return fmt.Sprintf("context %q → %s", dp.context, dp.server)
	}
	return dp.server
}
