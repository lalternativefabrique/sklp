// Package deployer is the LOCAL deploy use case: a leaf above deploy/control/
// controller (it imports them; they never import it — no cycle). It mirrors
// runner.Run: typed inputs, injectable streams, no os.Exit, returns error.
package deployer

import (
	"io"
)

// UpConfig is the typed input for a local deploy. The CLI adapter (and, later,
// the programmatic API) fills it; no flags or globals leak into the use case.
type UpConfig struct {
	Stack  string // stack name, e.g. "core" (informational this step)
	File   string // absolute path to the resolved deploy.yaml
	Space  string // named/ephemeral space (prod/staging/pr-NNNN); orthogonal to Stack
	Repo   string // repository root the file is relative to
	Listen string // optional TCP addr for the network control API (e.g. ":9443")
	Force  bool   // adopt a live space owned by a DIFFERENT stack (else the cross-stack re-apply is refused)
	// GatewayMode is THIS node's front-door mode (singleton | per-space), from
	// `sklet apply --gateway-mode`. It is never read from deploy.yaml: the mode
	// is a node-level property, not something a deploying app's config controls.
	GatewayMode string
	Stdout      io.Writer // nil -> os.Stdout
	Stderr      io.Writer // nil -> os.Stderr
}
