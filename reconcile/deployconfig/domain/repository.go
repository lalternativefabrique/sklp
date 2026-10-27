package domain

import "context"

// Repository persists the deployments (deploy.yaml -> node/space bindings)
// of a stack.
type Repository interface {
	Save(ctx context.Context, d *Deployment) error
	Update(ctx context.Context, d *Deployment) error
	// FindBySpace resolves the deployment that owns a space. The space is the
	// unit deployed — the node applies per space and refuses a second stack
	// claiming one — and it is unique whichever repo declares it, unlike the
	// manifest path, which several repos of a stack conventionally share.
	FindBySpace(ctx context.Context, stackID, space string) (*Deployment, error)
	FindByStackID(ctx context.Context, stackID string) ([]*Deployment, error)
	// FindApplied returns every deployment whose manifest was applied, least
	// recently checked first. It deliberately includes the ones already ready:
	// a space that converged can fall over later, and a watcher that only ever
	// looked at not-yet-ready deployments could never notice.
	FindApplied(ctx context.Context) ([]*Deployment, error)
	Delete(ctx context.Context, id string) error
}
