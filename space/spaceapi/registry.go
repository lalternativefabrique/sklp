// Package spaceapi is the transport-agnostic BUSINESS seam for inspecting and
// managing spaces (the runc-backed execution units under
// <cacheRoot>/spaces/<name>/). It is the single place the logic of
// ls / get / up / down / exec / purge lives, so BOTH transports call it:
//
//   - the CLI (apps/sklp/cmd/sklp/space_*.go) parses flags via the cli façade
//     and calls a Registry — it renders, it does not compute.
//   - an HTTP API (the skalpai UI control plane) parses a request and calls the
//     SAME Registry — it serializes JSON, it does not duplicate the logic.
//
// This mirrors what deploy already has via deploy/control.Supervisor: one
// interface, two transports, zero duplication. The runc implementation lives in
// package runtime (it owns spaceScope, the runc binary, the state roots); this
// package declares only the contract + DTOs so the CLI and a future HTTP server
// depend on an interface, not on the runtime internals.
//
// Frontier with deploy: a Registry reports the CONTAINER-level truth read from
// the runc state (always available, no supervisor needed) — name, status,
// containers, volumes. Application-level truth (readiness, captured logs,
// restart) stays behind deploy/control.Supervisor, because only a supervised
// deploy has it. `space` never grows a supervisor; if you need logs/readiness,
// you are describing a deploy.
package spaceapi

import (
	"context"
	"encoding/json"
	"io"
)

// SpaceInfo is one space as seen from the runc state + filesystem — the DTO both
// transports render. It is JSON-tagged so the HTTP adapter can encode it directly.
type SpaceInfo struct {
	// Name is the space identity (the <name> in spaces/<name>/).
	Name string `json:"name"`
	// Status is the aggregate runc status: "running" if any container runs,
	// "stopped" if state exists but nothing runs, "" if unknown.
	Status string `json:"status"`
	// Containers are this space's runc containers (the instances).
	Containers []ContainerInfo `json:"containers"`
	// Volumes are the named volume directories materialised for this space
	// (spaces/<name>/volumes/*), reported so `down -v` / `get` can show what
	// data would be destroyed.
	Volumes []string `json:"volumes,omitempty"`
	// Supervised is true when a deploy control socket is live for this space —
	// i.e. it is a deploy, not a bare space. Lets `ls` flag the deploy rows and
	// lets the CLI point the user at `deploy ls` for richer (readiness/logs) info.
	Supervised bool `json:"supervised"`
	// Repo is the host repo path bind-mounted at /workspace for this space's
	// containers, recovered from the OCI bundle. Lets a global view (the cockpit)
	// map a space back to its repo — e.g. to tail that repo's central CI log —
	// without the space carrying the path itself. "" when no container exposes it.
	Repo string `json:"repo,omitempty"`
}

// ContainerInfo is one runc container in a space (a service instance).
type ContainerInfo struct {
	ID     string `json:"id"`     // runc id (e.g. "sklp-deploy-<space>-<svc>-0")
	Status string `json:"status"` // runc status: running|stopped|created|paused
}

// UpSpec is the input to Up: enough to materialise and launch a named space.
// It deliberately mirrors what `sklp space up` needs; the runc implementation
// resolves the recipe, builds the rootfs (cache-by-hash), and starts a detached
// init the user attaches to via Exec. Kept minimal per the YAML-core-minimal
// rule — richer service topology is a deploy, not a space.
type UpSpec struct {
	// Repo is the project root holding .sklp/space.yaml (the recipe).
	Repo string
	// BaseImage overrides the recipe base when it declares none (env-defaulted
	// by the caller: SKLP_DEV_BASE_IMAGE). Empty = recipe/default decides.
	BaseImage string
	// Image, when set, opens a BARE space straight from that OCI image: the rootfs
	// is the pulled image (writable via the per-service overlay), with NO recipe
	// build and NO space.yaml required — `docker run`-style. Takes precedence over
	// a recipe when both are present.
	Image string
	// File is an explicit path to the space recipe (space.yaml), overriding the
	// Repo/.sklp lookup. Ignored when Image is set.
	File string
}

// ExecOpts controls an Exec into a space.
type ExecOpts struct {
	// Cmd is the argv to run; empty means an interactive shell ("sh").
	Cmd []string
	// TTY requests a PTY (interactive shell). The CLI sets it when stdin/stdout
	// are terminals; the HTTP transport leaves it false (it streams bytes).
	TTY bool
	// Container selects WHICH of the space's containers to enter, as the service
	// name (`core`) or the full runc id (`sklp-deploy-<space>-core`). A dev/ci/
	// deploy space runs several service containers, so "the space" is not one
	// target: without this the caller gets whichever one happens to come first.
	// Empty keeps that default (the `space up` init, else the first running one).
	Container string
}

// Registry is the business seam: every space operation, transport-free. The runc
// implementation is constructed in package runtime; the CLI and the HTTP server
// receive it as this interface.
//
// All methods are scoped to the host's space cache. Errors are returned, never
// printed and never os.Exit — the transport decides how to surface them.
type Registry interface {
	// List returns every space found under the cache (running or stopped),
	// sorted by name. The container-level view that makes dev/space/deploy/ci
	// spaces all visible — the gap `deploy ps` left.
	List(ctx context.Context) ([]SpaceInfo, error)

	// Get returns one space's detail. ErrSpaceNotFound when it does not exist.
	Get(ctx context.Context, name string) (SpaceInfo, error)

	// Up creates and launches a new detached space named `name`. It returns
	// ErrSpaceExists when a space of that name already exists (the user must
	// `down` it first) — `up` never silently re-creates or attaches.
	Up(ctx context.Context, name string, spec UpSpec) error

	// Down stops and removes a space, INCLUDING its named volumes — deleting a
	// space deletes the space. Keeping the volumes back made the space dir
	// survive, and since List enumerates those dirs the space came straight back
	// on the next refresh with no way left to remove it. A missing space is not
	// an error (idempotent teardown).
	Down(ctx context.Context, name string) error

	// Stop halts a space's containers and tears its network down, but KEEPS the
	// space dir and its named volumes — the non-destructive half of Down, so a
	// database survives and `up` brings the space back with its data. The space
	// stays listed with Status "stopped". ErrSpaceNotFound when there is no such
	// space: unlike Down (an idempotent teardown), stopping something that does
	// not exist is a mistake worth reporting, not a silent success.
	Stop(ctx context.Context, name string) error

	// Exec runs a command (or an interactive shell) inside a live space and
	// returns the command's exit code. ErrSpaceNotFound / ErrSpaceNotRunning
	// when there is nothing to exec into. The CLI proxies the user's stdio; the
	// HTTP transport proxies the request stream.
	Exec(ctx context.Context, name string, opts ExecOpts, io ExecIO) (int, error)

	// Purge tears down every space that has no running container (the GC for the
	// dev/ci leak), volumes included — same contract as Down. Returns the names
	// reaped so the transport can report them.
	Purge(ctx context.Context) ([]string, error)
}

// ExecIO is the stdio the transport supplies to Exec, so the Registry stays
// ignorant of whether bytes come from a terminal or an HTTP stream.
type ExecIO struct {
	Stdin  io.Reader
	Stdout io.Writer
	// Rows/Cols size the PTY when ExecOpts.TTY is set (0 = leave default).
	Rows, Cols uint16
}

// DecodeSpaceInfos parses an inventory payload (the opaque JSON a node serves
// at GET /v1/spaces) back into typed rows. It lives here because this package
// owns the wire shape: the control client carries the bytes without depending
// on these types, and every reader decodes them the same way.
func DecodeSpaceInfos(raw []byte) ([]SpaceInfo, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var out []SpaceInfo
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}
