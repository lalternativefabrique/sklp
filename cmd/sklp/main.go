// sklp is the developer-facing CLI for the dev → CI → publish lifecycle.
//
// Verbs:
//
//	sklp init        — onboard a repo: detect the dev's AI assistant and
//	                   install repo-tailored skills, or write a baseline
//	sklp run <task>  — run .sklp/pipelines/<task>.yaml in-process via the runner package
//	sklp dev <stack> — run a stack's local dev environment (.sklp/<stack>/dev.yaml)
//
// The CLI itself runs no LLM: onboarding is deterministic and the
// reasoning belongs to the developer's own assistant. The runner package
// (pipeline) is the single source of truth for task execution.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/internal/layout"
	"github.com/lalternativefabrique/sklp/internal/limarelay"
	"github.com/lalternativefabrique/sklp/internal/plan"
	"github.com/lalternativefabrique/sklp/pipeline"
)

const usage = `sklp — dev → CI → publish runner

Usage:
  sklp <verb> [flags...]
  sklp -v | --version

Verbs:
  init        Onboard a repo: detect your AI assistant, install repo-tailored
              skills, or write a deterministic baseline (no LLM, no API key)
  skills      Generate AI-assistant skills (Claude, Cursor, Codex, …) for this repo
  run         Run a pipeline from .sklp/pipelines/<name>.yaml (or -file <path>)
  dev         Run a stack's local dev environment (.sklp/<stack>/dev.yaml) —
              long-running services. 'sklp dev' with no stack lists the choices;
              'sklp dev down' stops a stack started in another terminal.
  space       Manage closed environments built from .sklp/space.yaml (base image
              + toolchain + packages): 'space up <name>' creates + launches a
              detached space and drops you into a shell; 'space ls' / 'get
              <name>' inspect; 'space down <name> [-v]' tears one down (-v also
              drops its volumes); 'space exec <name> [-- cmd]' re-enters a live
              space; 'space purge [-v]' reaps idle spaces; 'space build' only
              builds the rootfs. Bare 'space' / 'space -- <cmd>' open the default
              interactive space. Requires the embedded_runtime build.
  deploy      Pilot a node. 'deploy apply <stack> -space <s>' applies
              .sklp/<stack>/deploy.yaml to the active context's node ('deploy
              apply' with no stack lists the choices). A bare 'sklp deploy
              <stack>' still works and says so. Requires the embedded_runtime
              build.
              'deploy ls' lists running instances; 'deploy get <name>' shows one
              deploy's detail; 'deploy logs <svc> [-f]' tails a service's
              container output (read-only). 'deploy mesh ls' lists the
              WireGuard peers the node's meshauthority knows; 'deploy mesh rm
              <id>' forgets a dead one (a rebuilt node's old identity).
              REMOTE (recommended): supervise with '--listen :9443' to expose
              the control API over TLS + a Bearer token, then pilot it from any
              machine with '--server https://host:9443 --token <T>
              --ca-fingerprint <FP>' (or --insecure on a LAN). 'deploy token' /
              'deploy fingerprint' print the values to copy. No SSH needed.
              CONTEXT (kubeconfig-style): 'deploy login --context <p> --server ...
              --space ...' saves a context; 'deploy context use <p>' makes it
              active. A plain 'sklp deploy <stack>' then applies the LOCAL
              manifest to the active context's server over the network (nothing
              is copied). A context with no server, or '--local', uses the socket.
              REMOTE (deprecated): '--host user@server' runs the deploy ON that
              server over ssh; prefer --server. Flags: --ssh-port N,
              --identity <key>, --remote-bin <path> ($SKLP_REMOTE_BIN).
  gateway     Manage the host-level ingress gateway singleton (the shared
              :443/:80 front door across spaces): 'gateway up' / 'down' /
              'status'. Driver selected by deploy.yaml gateway.driver (default
              traefik). Requires the embedded_runtime build.
  auth        Manage CLI authentication: login / status / logout
  issue       Manage project issues from the terminal (list, new, show, edit,
              status, promote, gh-link / gh-close / gh-reopen / gh-unlink)
  flag        Manage a project's feature flags (list, enable, disable)
  cache       Inspect the repo cache state / collect the CI tool cache
              ('cache status', 'cache gc')
  clean       Free disk space: rebuildable caches + dead runtime state.
              '--images' also drops OCI images, '--deep' idle spaces, '-v'
              named volumes (data). '--dry-run' previews without deleting.
  runner      Build the project runner image from .sklp/space.yaml
  version     Print the sklp binary version (use -short for the bare tag)
  completion  Emit shell completion script (bash|zsh|fish)

Examples:
  sklp init                      # detect assistant, install skills (or baseline)
  sklp init -agent none          # deterministic baseline (config + dev + ci draft)
  sklp skills                    # generate Claude + Cursor skills for this repo
  sklp run                       # list available tasks
  sklp run ci
  sklp run publish -dry-run
  sklp run -file path/to/foo.yaml
  sklp dev                       # list the dev-capable stacks
  sklp dev core                  # run .sklp/core/dev.yaml
  sklp dev down                  # stop the running dev stack (any terminal)
  sklp space up dev              # create + launch a space named 'dev', open a shell
  sklp space ls                  # list all spaces
  sklp space exec dev -- go test ./...  # one-shot command in the 'dev' space
  sklp space down dev -v         # tear 'dev' down, dropping its volumes too
  sklp space                     # shell inside the default space from .sklp/space.yaml
  sklp deploy                    # list the deploy-capable stacks
  sklp deploy core -space production
  sklp deploy transcript         # deploy .sklp/transcript/deploy.yaml (default space)
  sklp deploy core -space pr-1234  # materialise an ephemeral preview space
  sklp deploy delete -space pr-1234  # delete the space (stop, reap, remove its manifest)
  sklp deploy ls                 # list running instances of the deploy
  sklp deploy get production     # show the 'production' deploy's detail
  sklp deploy logs api -f        # follow the 'api' service container output
  sklp deploy -space production --listen :9443 # serve the control API on a port
  sklp deploy ls --server https://vm:9443 --token <T> --ca-fingerprint <FP>
  sklp deploy login --context prod --server https://vm:9443 --token <T> --ca-fingerprint <FP> --space production
  sklp deploy context use prod   # make 'prod' the active deploy target
  sklp deploy core               # apply LOCAL manifest to 'prod' over the network (no copy)
  sklp deploy core --local       # force the local socket instead
  sklp deploy --host me@vm -space production   # deploy ON vm over ssh (deprecated)
  sklp issue list -project p1
  sklp issue new "panic on startup" -type bug -body "stack trace..."
  sklp version
  source <(sklp completion zsh)

Run 'sklp <verb> -h' for verb-specific flags.
`

func main() {
	// Fast path for the FS-confinement re-exec shim: when sklp was
	// re-exec'd to apply Landlock before running a step's command, this
	// applies the ruleset and execs the real command (never returns).
	// On a normal invocation it returns immediately.
	plan.MaybeRunSandboxShim()

	// Fast path for the space network worker: when sklp was re-exec'd by
	// spacenet into a fresh user+net namespace to own a space bridge, this
	// becomes that worker (or a netns holder) and never returns. No-op on a
	// normal invocation and off the space-deploy build.
	maybeRunSpaceNetWorker()

	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	root := rootCommand()

	// Root-level special tokens keep their curated output, matching the legacy
	// switch: `-h`/`--help`/`help` print the banner to stdout; `-v`/`--version`
	// route to the version printer (the flag form of `sklp version`).
	switch os.Args[1] {
	case "-h", "--help", "help":
		fmt.Fprint(os.Stdout, usage)
		return
	case "-v", "--version":
		runVersion(os.Args[2:])
		return
	}

	// Unknown verb (a non-flag token with no matching command): preserve the
	// legacy "unknown verb" banner + exit 2 instead of the generated group help.
	if tok := os.Args[1]; len(tok) > 0 && tok[0] != '-' && findRootSub(root, tok) == nil {
		fmt.Fprintf(os.Stderr, "sklp: unknown verb %q\n\n%s", tok, usage)
		os.Exit(2)
	}

	// Early transport shims for `deploy`, kept AHEAD of façade dispatch: the SSH
	// (`--host`) and network-control (`--server`) clients are non-gated, so a
	// binary without the embedded runtime can still pilot a remote server. They
	// run before Execute so the verbatim deploy tail is inspected exactly as
	// before; no transport flag => handled is false and dispatch continues.
	if os.Args[1] == "deploy" {
		os.Args = append(os.Args[:2], normalizeDeployApply(os.Args[2:])...)
		if handled, code := maybeRunRemote(os.Args[2:]); handled {
			os.Exit(code)
		}
		if handled, code := maybeRunRemoteNet(os.Args[2:]); handled {
			os.Exit(code)
		}
		// Plain `sklp deploy [stack]`: route to the remote apply when the active
		// context is remote (kubectl-style). Local contexts fall through unchanged.
		if handled, code := maybeRunRemoteDeploy(os.Args[2:]); handled {
			os.Exit(code)
		}
	}

	adapter := cli.NewFlagAdapter()
	os.Exit(adapter.Execute(context.Background(), root, os.Args[1:]))
}

// findRootSub reports the root command matching tok (by Name or Alias), or nil.
// Used to keep the legacy "unknown verb" banner for tokens the tree does not
// declare, instead of the generated per-group usage.
func findRootSub(root cli.Command, tok string) *cli.Command {
	for i := range root.Sub {
		c := &root.Sub[i]
		if c.Name == tok {
			return c
		}
		for _, al := range c.Aliases {
			if al == tok {
				return c
			}
		}
	}
	return nil
}

// runRunVerb is the single entry point for executing a .sklp/pipelines/
// YAML. It accepts three call shapes:
//
//	sklp run                  — list available tasks and exit 0.
//	sklp run <name> [flags]   — resolve to .sklp/pipelines/<name>.yaml.
//	sklp run -file <path>     — use <path> verbatim (escape hatch for
//	                            ad-hoc / out-of-tree pipelines).
//
// All other flags (`-repo`, `-base-ref`, `-output json`, …) are
// passed straight through to pipeline.Main, which keeps the
// programmatic API (core/ci spawner) unchanged.
func runRunVerb(args []string) {
	// `--listen` turns this invocation into the node run API instead of a
	// one-shot pipeline: the process blocks serving requests. Intercepted
	// before anything else so it never reaches the task-name parsing.
	if opts, install, ok := parseListenArgs(args); ok {
		if install {
			os.Exit(runServeInstall(opts))
		}
		os.Exit(runServe(opts))
	}

	// `sklp run` with no positional task and no `-file` lists the tasks
	// it would offer if invoked with one. The user gets a one-shot
	// catalogue without having to remember `ls .sklp/tasks/`.
	if len(args) == 0 {
		listTasks(os.Stdout)
		return
	}

	// Extract `-file <path>` ourselves before delegating to the runner
	// so the user can pass a path that doesn't live under .sklp/tasks/.
	// Anything else stays in `rest` and reaches pipeline.Main verbatim.
	taskFile, name, rest := extractTaskFile(args)
	if taskFile == "" && name == "" {
		// Neither a positional name nor `-file` — treat as "list mode"
		// even when extra flags are present, so `sklp run -repo .`
		// behaves predictably rather than hitting an obscure flag
		// parser error inside the pipeline.
		listTasks(os.Stdout)
		return
	}
	if taskFile == "" {
		if builtin, ok := strings.CutPrefix(name, "sklp:"); ok {
			name = builtinFilePrefix + builtin
		}
		taskFile = filepath.Join(layout.New(".").PipelinesDir(), name+".yaml")
	}
	if _, err := os.Stat(taskFile); err != nil && !willSelfClone(rest) {
		builtin := strings.TrimPrefix(strings.TrimSuffix(filepath.Base(taskFile), ".yaml"), builtinFilePrefix)
		if path, ok := materializeBuiltin(builtin); ok {
			fmt.Fprintf(os.Stderr, "sklp run: no %s, using built-in sklp:%s\n", taskFile, builtin)
			taskFile = path
		}
	}
	// Skip the early existence check when the runner is going to clone
	// the repo itself (k8s spawner case): the cwd at this point is the
	// empty Job workspace, so the task file legitimately doesn't exist
	// yet. pipeline.Main will resolve the path against -repo after the
	// clone completes (see pipeline.go:taskAbs).
	if !willSelfClone(rest) {
		if _, err := os.Stat(taskFile); err != nil {
			fmt.Fprintf(os.Stderr, "sklp run: task file %s not found\n", taskFile)
			listTasks(os.Stderr)
			os.Exit(2)
		}
	}

	// Pipelines run in the space runtime, which only exists on Linux. On a
	// Mac, relay the whole invocation into the VM the way `sklp dev` does —
	// the relay also carries the secrets passphrase and tokens the guest
	// cannot resolve itself. Running "locally" on darwin would silently fall
	// back to the host and vet/test a platform CI never targets.
	if runtime.GOOS == "darwin" {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, "sklp run:", err)
			os.Exit(1)
		}
		err = limarelay.RelaySelf(cwd)
		if err == nil {
			return
		}
		var exit *limarelay.GuestExitError
		if errors.As(err, &exit) {
			os.Exit(exit.Code)
		}
		fmt.Fprintln(os.Stderr, "sklp run: relay:", err)
		os.Exit(1)
	}

	runRunnerVerb(taskFile, rest)
}

// willSelfClone reports whether the passthrough args carry a non-empty
// `-repo-url` flag, signalling that pipeline.Main will materialise the
// workspace itself before resolving the task file. Used to suppress the
// early existence check in runRunVerb so a k8s Job (whose workspace
// starts empty) doesn't bail before the clone happens.
//
// The flag shapes we accept mirror Go's `flag` package: `-repo-url X`,
// `--repo-url X`, `-repo-url=X`, `--repo-url=X`. An empty value is
// treated as "not set" — matching pipeline.Main's own check.
func willSelfClone(args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-repo-url" || a == "--repo-url":
			if i+1 < len(args) && args[i+1] != "" {
				return true
			}
		case strings.HasPrefix(a, "-repo-url=") || strings.HasPrefix(a, "--repo-url="):
			_, v, _ := strings.Cut(a, "=")
			if v != "" {
				return true
			}
		}
	}
	return false
}

// extractTaskFile peels the first non-flag positional argument off
// `args` (interpreted as the task name) OR a `-file <path>` /
// `--file <path>` pair (interpreted as an explicit path). Returns the
// remaining args untouched so flag.Parse downstream still sees a clean
// slate.
func extractTaskFile(args []string) (taskFile, name string, rest []string) {
	rest = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-file" || a == "--file":
			if i+1 < len(args) {
				taskFile = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "-file=") || strings.HasPrefix(a, "--file="):
			_, taskFile, _ = strings.Cut(a, "=")
		case !strings.HasPrefix(a, "-") && name == "" && taskFile == "":
			// First bare token is the task name. Subsequent bare tokens
			// (if any) keep flowing through to the runner; flag.Parse
			// in pipeline.Main treats unknown trailing args as positional
			// inputs the user can act on if needed.
			name = a
		default:
			rest = append(rest, a)
		}
	}
	return taskFile, name, rest
}

// listTasks prints the runnable pipelines as a deterministic catalogue,
// delegating the tree walk (canonical .sklp/pipelines, legacy .sklp/tasks
// fallback, '_'-template exclusion) to the layout package — the single source
// of truth. Output is stable so it can be diffed in tests and piped into shell
// completion.
func listTasks(w *os.File) {
	lay := layout.New(".")
	dir := lay.PipelinesDir()
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		fmt.Fprintf(w, "sklp run: no pipelines directory at %s (run `sklp init` to bootstrap)\n", dir)
		return
	}
	names, err := lay.Pipelines()
	if err != nil {
		fmt.Fprintf(w, "sklp run: no pipelines directory at %s (run `sklp init` to bootstrap)\n", dir)
		return
	}
	if len(names) == 0 {
		fmt.Fprintf(w, "sklp run: no runnable pipelines in %s\n", dir)
		return
	}
	fmt.Fprintln(w, "Available pipelines (run with `sklp run <name>`):")
	for _, n := range names {
		fmt.Fprintf(w, "  %s\n", n)
	}
}

// runRunnerVerb hands the resolved task off to the in-process runner
// package via pipeline.ParseArgsAndRun. It injects `-task <file>` (so the
// runner reads the right YAML) and defaults `-repo .` when the caller
// omitted it, then maps a run error to a non-zero exit code. No os.Args
// mutation: the args slice is passed directly.
func runRunnerVerb(taskFile string, args []string) {
	// Inject -task / -repo defaults if absent so the user doesn't have
	// to repeat them every call. Existing values take precedence.
	hasFlag := func(name string) bool {
		for _, a := range args {
			if a == "-"+name || a == "--"+name {
				return true
			}
		}
		return false
	}
	full := append([]string{}, args...)
	if !hasFlag("task") {
		full = append([]string{"-task", taskFile}, full...)
	}
	if !hasFlag("repo") {
		full = append(full, "-repo", ".")
	}

	ctx := context.Background()
	// A HOST run spawns detached `runc run -d` services whose only reaper is the
	// deferred teardown in the space path. Go's default disposition kills the
	// process outright on SIGINT/SIGTERM (Ctrl-C, a `timeout` wrapper), so no
	// defer runs and every service is orphaned into a cgroup runc then refuses
	// to reuse. Cancelling the context instead lets the run unwind and tear down.
	//
	// The INNER run (this same binary, re-executed inside the runner container)
	// must keep the default disposition: it owns no detached container, and it
	// runs as pid 1, where installing a handler flips SIGTERM from "ignored" to
	// "delivered" and changes how the container dies.
	if os.Getenv(pipeline.RunnerModeEnv) == "" {
		signalCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()
		ctx = signalCtx
	}

	if _, err := pipeline.ParseArgsAndRun(ctx, full); err != nil {
		fmt.Fprintln(os.Stderr, "sklp run:", err)
		os.Exit(pipeline.ExitCode(err))
	}
}

// builtinFilePrefix keeps the in-space re-entry, which only receives the
// file's base name, from resolving to a repo's own pipeline of the same name.
const builtinFilePrefix = "sklp-builtin-"

func materializeBuiltin(name string) (string, bool) {
	raw, ok := pipeline.Builtin(name)
	if !ok {
		return "", false
	}
	dir, err := os.MkdirTemp("", "sklp-builtin-")
	if err != nil {
		return "", false
	}
	path := filepath.Join(dir, builtinFilePrefix+name+".yaml")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return "", false
	}
	return path, true
}
