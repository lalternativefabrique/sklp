// Package embed carries the external OCI helper binaries (runc, slirp4netns)
// embedded into the sklp binary so the embedded runtime needs nothing on PATH.
//
// The binaries themselves are NOT committed. Run `go generate ./...` in this
// directory (or ./fetch.sh) to download + checksum-verify them into this
// folder before a `-tags embedded_runtime` build. See README.md.
//
// The pinned descriptors live in the embed-free subpackage ./meta so the gen
// tool can read them WITHOUT compiling this //go:embed-bearing package (which
// can't compile until the binaries exist — the bootstrap cycle meta breaks).
//
//go:generate go run ./gen
package embed

import "github.com/lalternativefabrique/sklp/space/embed/meta"

// Re-exported for any in-tree consumer that wants the pins from the embed
// package directly. The single source of truth is ./meta.
const (
	RuncVersion  = meta.RuncVersion
	SlirpVersion = meta.SlirpVersion
)

// Assets is the helper-binary descriptor list (see meta.Assets).
var Assets = meta.Assets
