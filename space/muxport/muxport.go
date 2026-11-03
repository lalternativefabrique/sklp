// Package muxport derives a dev worktree's host mux port. It has no
// dependencies so the Mac side, which cannot build the space runtime, derives
// the same port the guest binds.
package muxport

// Base is the bottom of the unprivileged range the per-worktree mux port is
// derived into: a high port, so the bind needs no root.
const Base = 8080

// Span caps the derived range [Base, Base+Span).
const Span = 1000

// For derives the stable per-worktree port from a scope id. Same worktree, same
// port; empty scope falls back to Base.
func For(scope string) int {
	if scope == "" {
		return Base
	}
	var acc uint32
	for i := 0; i < len(scope); i++ {
		acc = acc*31 + uint32(scope[i])
	}
	return Base + int(acc%Span)
}
