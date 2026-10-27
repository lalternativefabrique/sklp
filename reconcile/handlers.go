package reconcile

import (
	"context"

	deployapply "github.com/lalternativefabrique/sklp/reconcile/deployconfig/apply"
	imagesync "github.com/lalternativefabrique/sklp/reconcile/imageupdate/sync"
)

// handlers.go adapts the Git handlers to the loop.
//
// Both were written for the webhook, which carried a stack id, a project, a
// user and a GitHub token — everything core knew about the push. A node has
// none of that: it pulls, so there is no caller to inherit an identity from.
//
// What it has instead is itself. The hostname is the scope every KV key hangs
// off, which is the same role the stack id played: a node serves one node's
// worth of spaces, and nothing on it belongs to a second tenant.

// NodeScope is the identity a node reconciles under, standing where a webhook
// put the stack id. Anchoring on the hostname rather than inventing an id
// means the value an operator sees in a log is the machine they can ssh into.
type NodeScope string

// DeployReconciler applies what deploy-root.yaml declares.
type DeployReconciler struct {
	handler *deployapply.Handler
	scope   NodeScope
}

func NewDeployReconciler(handler *deployapply.Handler, scope NodeScope) *DeployReconciler {
	return &DeployReconciler{handler: handler, scope: scope}
}

// Reconcile implements Reconciler.
//
// ProjectID, UserID and GitToken stay empty: they exist for core's secret
// resolution and its GitHub client, and a node resolves secrets from its own
// manifest and reads a mirror it already authenticated to. Passing a blank
// where a value has no meaning beats inventing one that looks real in a log.
func (r *DeployReconciler) Reconcile(ctx context.Context, repo, ref string) error {
	return r.handler.Handle(ctx, deployapply.Command{
		StackID: string(r.scope),
		Repo:    repo,
		Ref:     ref,
	})
}

// WatchReconciler makes the node's image watches match image-watches.yaml.
type WatchReconciler struct {
	handler *imagesync.Handler
	scope   NodeScope
}

func NewWatchReconciler(handler *imagesync.Handler, scope NodeScope) *WatchReconciler {
	return &WatchReconciler{handler: handler, scope: scope}
}

// Reconcile implements Reconciler.
func (r *WatchReconciler) Reconcile(ctx context.Context, repo, ref string) error {
	return r.handler.Handle(ctx, imagesync.Command{
		StackID: string(r.scope),
		Repo:    repo,
		Ref:     ref,
	})
}
