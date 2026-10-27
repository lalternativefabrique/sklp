package runnerbuild

import "os"

// StepEnvDebugVar gates the per-RUN environment dump. Off by default: it prints
// the resolved PATH and toolchain locations from inside the build rootfs, which
// is the only vantage point that shows which `go` a step actually selected.
const StepEnvDebugVar = "SKLP_DEBUG_STEPENV"

const stepEnvDebugScript = `echo "=== [sklp stepenv] ==="; ` +
	`echo "[sklp stepenv] PATH=$PATH"; ` +
	`echo "[sklp stepenv] GOROOT=${GOROOT:-<unset>}"; ` +
	`echo "[sklp stepenv] GOPATH=${GOPATH:-<unset>}"; ` +
	`echo "[sklp stepenv] GOMODCACHE=${GOMODCACHE:-<unset>}"; ` +
	`echo "[sklp stepenv] pwd=$(pwd 2>&1)"; ` +
	`echo "[sklp stepenv] id=$(id 2>&1)"; ` +
	`echo "[sklp stepenv] go(first)=$(command -v go 2>&1 || echo none)"; ` +
	`for d in $(echo "$PATH" | tr ':' ' '); do [ -x "$d/go" ] && echo "[sklp stepenv] go(on PATH)=$d/go -> $(readlink -f "$d/go" 2>&1)"; done; ` +
	`echo "[sklp stepenv] go(resolved)=$(readlink -f "$(command -v go 2>/dev/null)" 2>&1 || echo none)"; ` +
	`echo "[sklp stepenv] go(env GOROOT)=$(go env GOROOT 2>&1 || true)"; ` +
	`echo "[sklp stepenv] go(version)=$(go version 2>&1 || true)"; ` +
	`echo "[sklp stepenv] /usr/local/go/bin/go=$(ls -la /usr/local/go/bin/go 2>&1 || echo absent)"; ` +
	`echo "[sklp stepenv] /opt/sklp/tools/bin/go=$(ls -la /opt/sklp/tools/bin/go 2>&1 || echo absent)"; ` +
	`echo "[sklp stepenv] proc/self/exe=$(readlink -f /proc/self/exe 2>&1 || echo none)"; ` +
	`echo "=== [sklp stepenv done] ==="; `

// stepEnvDebugPrefix returns a shell snippet to run before a step, or "" when
// the debug var is unset.
func stepEnvDebugPrefix() string {
	if os.Getenv(StepEnvDebugVar) == "" {
		return ""
	}
	return stepEnvDebugScript
}
