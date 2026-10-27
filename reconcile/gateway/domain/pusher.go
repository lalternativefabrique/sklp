package domain

import "context"

// Pusher converges a node's live front door toward a route set. It is the
// domain's view of the node control API: an interface here keeps the
// application layer free of HTTP, and lets a node be a pod or a VM without the
// use cases knowing which.
//
// Push replaces the WHOLE set — routes absent from the argument are dropped —
// so a caller always sends the node's complete desired document.
type Pusher interface {
	Push(ctx context.Context, node *Node, routes []*Route) error
}
