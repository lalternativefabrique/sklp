package devstack

// Programmatic entry point for the dev stack — modelled on
// runner.Run(ctx, RunConfig): typed inputs, injectable Stdout/Stderr,
// no os.Exit, returns an error the adapter (cmd/sklp) maps to an exit
// code. Main stays as the thin flag-parsing shim over Up.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

// DevConfig is the library-friendly input to Up. Every field maps to an
// equivalent `sklp dev` flag; the CLI adapter resolves defaults (repo
// root, dev.yaml path) before calling.
type DevConfig struct {
	Stack    string    // stack name, e.g. "core" — scopes per-stack resources
	TaskPath string    // absolute path to the resolved dev.yaml
	Repo     string    // repo root (absolute)
	Validate bool      // parse + validate only, launch nothing
	Check    bool      // with Validate: report auto-fixes without writing
	NoColor  bool      // disable colored log prefixes
	GraceMs  int       // ms between SIGTERM and SIGKILL on shutdown
	Only     []string  // when non-empty: restrict to these services + their transitive deps
	Stdout   io.Writer // nil -> os.Stdout
	Stderr   io.Writer // nil -> os.Stderr
}

// devError carries an exit code so the CLI shim maps Up's error to the
// exact code Main used to return (1 launch/validation, 2 input).
type devError struct {
	code int
	msg  string
}

func (e *devError) Error() string { return e.msg }

func devErr(code int, format string, a ...any) error {
	return &devError{code: code, msg: fmt.Sprintf(format, a...)}
}

// devExitCode maps an Up error to a process exit code. nil -> 0.
func devExitCode(err error) int {
	if err == nil {
		return 0
	}
	var de *devError
	if errors.As(err, &de) {
		return de.code
	}
	return 1
}

// Up runs the dev stack described by cfg. No os.Exit; returns an error
// the caller maps to an exit code. Honors ctx for cancellation.
func Up(ctx context.Context, cfg DevConfig) error {
	stdout := cfg.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := cfg.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	repoAbs := cfg.Repo
	taskPath := cfg.TaskPath

	// A validate-only run launches nothing, so it gets no run-log.
	if !cfg.Validate {
		var closer io.Closer
		var logPath string
		stdout, stderr, closer, logPath = teeRunLog(stdout, stderr, repoAbs, cfg.Stack)
		if closer != nil {
			defer closer.Close()
			fmt.Fprintf(stdout, "sklp dev: run-log %s\n", logPath)
		}
	}

	if cfg.Validate {
		// --check disables the auto-fix writeback so CI / pre-commit
		// hooks can flag a typo without silently mutating the file
		// under the user. Without --check, --validate behaves like a
		// dry-run that DOES allow auto-fix to repair the file (same
		// behavior as a real launch, minus the launch).
		stack, err := LoadOpts(taskPath, !cfg.Check)
		if err != nil {
			return devErr(1, "%v", err)
		}
		names := make([]string, 0, len(stack.Services))
		for _, ns := range stack.Services {
			names = append(names, ns.Name)
		}
		fmt.Fprintf(stdout, "✓ %s — %d service(s), launch order: %s\n",
			taskPath, len(stack.Services), strings.Join(names, " → "))
		return nil
	}

	stack, err := Load(taskPath)
	if err != nil {
		return devErr(1, "sklp dev: %v", err)
	}

	if len(cfg.Only) > 0 {
		if err := filterServices(stack, cfg.Only); err != nil {
			return devErr(2, "sklp dev: --only: %v", err)
		}
	}

	// Resolve env files relative to the repo root.
	resolvedEnvFiles := make([]string, 0, len(stack.EnvFiles))
	for _, p := range stack.EnvFiles {
		if !filepath.IsAbs(p) {
			p = filepath.Join(repoAbs, p)
		}
		resolvedEnvFiles = append(resolvedEnvFiles, p)
	}
	envFromFiles, err := loadEnvFiles(resolvedEnvFiles)
	if err != nil {
		return devErr(1, "sklp dev: load env files: %v", err)
	}

	names := make([]string, 0, len(stack.Services))
	for _, s := range stack.Services {
		names = append(names, s.Name)
	}
	color := !cfg.NoColor && stdoutColorEnabled()
	printer := newLogPrinter(stdout, names, color)
	printer.Banner("sklp dev — starting %d service(s) from %s", len(stack.Services), taskPath)

	// One root context cancelled by SIGINT/SIGTERM, the caller's ctx, or
	// the first service exit. exec.CommandContext uses this to kill the
	// shell itself if anything bypasses our explicit stop path.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)

	// `sklp dev` runs every service in the embedded runc runtime + spacenet —
	// the SAME runtime sklp deploy and the CI runner use (dev↔CI↔deploy parity).
	// This is the ONLY backend: there is no docker / process-host fallback. A
	// host that cannot run rootless runc (no userns) fails loudly rather than
	// silently degrading to a non-isolated, non-parity launch.
	space := devSpaceName(repoAbs, cfg.Stack)
	backend, ok := ChooseDevBackend(space, stack.AllowHost)
	if !ok {
		// Off Linux, relayDevLima replaces this process with the same
		// invocation inside a Lima VM and never returns.
		if err := relayDevLima(cfg); err != nil {
			return devErr(1, "sklp dev: embedded runc runtime unavailable — %v", err)
		}
		return devErr(1, "sklp dev: lima relay returned without exec")
	}
	printer.Banner("sklp dev — embedded runc runtime active")
	baseEnv := mergeEnv(map[string]string{}, stack.Env)
	if code := embeddedRun(runCtx, cancel, backend, space,
		stack, repoAbs, cfg.Stack, envFromFiles, baseEnv, printer, sigs, cfg.GraceMs); code != 0 {
		return devErr(code, "")
	}
	return nil
}
