package domain

import "context"

// NodeRepository persists the gateway nodes of a project.
type NodeRepository interface {
	Save(ctx context.Context, node *Node) error
	Update(ctx context.Context, node *Node) error
	FindByID(ctx context.Context, stackID, id string) (*Node, error)
	FindByName(ctx context.Context, stackID, name string) (*Node, error)
	ListByStack(ctx context.Context, stackID string) ([]*Node, error)
	Delete(ctx context.Context, stackID, id string) error
}

// RouteRepository persists a node's master route document.
type RouteRepository interface {
	Save(ctx context.Context, route *Route) error
	Update(ctx context.Context, route *Route) error
	FindByID(ctx context.Context, nodeID, id string) (*Route, error)
	FindByHostPath(ctx context.Context, nodeID, host, path string) (*Route, error)
	ListByNode(ctx context.Context, nodeID string) ([]*Route, error)
	Delete(ctx context.Context, nodeID, id string) error
}
