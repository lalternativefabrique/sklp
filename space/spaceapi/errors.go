package spaceapi

import "errors"

// Sentinel errors the Registry returns so each transport maps them to its own
// surface: the CLI to an exit code + message, the HTTP API to a status code.
var (
	// ErrSpaceNotFound: no space of that name exists under the cache.
	ErrSpaceNotFound = errors.New("space not found")
	// ErrSpaceExists: Up was asked to create a space that already exists; the
	// caller must `down` it first (up never re-creates silently).
	ErrSpaceExists = errors.New("space already exists")
	// ErrSpaceNotRunning: the space exists on disk but has no running container
	// to exec into.
	ErrSpaceNotRunning = errors.New("space is not running")
	// ErrContainerNotFound: ExecOpts.Container names no container in the space.
	// Distinct from ErrSpaceNotRunning: the space may be perfectly alive while
	// the requested target is a typo or a container that has since gone.
	ErrContainerNotFound = errors.New("container not found in space")
	// ErrContainerNotRunning: the named container exists but is not running (a
	// converged oneshot like migrate/seed), so there is no process to enter.
	ErrContainerNotRunning = errors.New("container is not running")
)
