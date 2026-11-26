package reconcile

import (
	"context"
	"time"

	"github.com/lalternativefabrique/sklp/reconcile/deployconfig/nodeapi"
	"github.com/lalternativefabrique/sklp/reconcile/imageupdate/domain"
	"github.com/lalternativefabrique/sklp/reconcile/imageupdate/rolling"
	"github.com/lalternativefabrique/sklp/reconcile/imageupdate/tags"
)

// StartImageRolling polls the registries of the watches this node keeps and
// rolls its own services onto newer tags, with no core in the loop. Rolls a
// previous run left unsettled are watched again first.
func StartImageRolling(ctx context.Context, stores *Stores, interval time.Duration, local *LocalNode) *rolling.Poller {
	node := localCredentials{inner: nodeapi.NewControlRoller(), local: local}
	roller := rolling.NewRoller(stores.Watches, node)
	roller.ResumePending(ctx)
	poller := rolling.NewPoller(stores.Watches, tags.NewClient(), roller, interval).WithLiveImages(node)
	poller.Start()
	return poller
}

// localCredentials points every call at this node's API with its current
// token: the token a watch kept when it was synced goes stale on rotation.
type localCredentials struct {
	inner *nodeapi.ControlRoller
	local *LocalNode
}

func (c localCredentials) refresh(w *domain.Watch) *domain.Watch {
	if c.local == nil {
		return w
	}
	fresh := *w
	fresh.NodeServer = c.local.ControlURL
	fresh.NodeToken = c.local.CurrentToken()
	fresh.NodeCAFingerprint = c.local.CAFingerprint
	return &fresh
}

func (c localCredentials) Roll(ctx context.Context, w *domain.Watch, tag string) (string, error) {
	return c.inner.Roll(ctx, c.refresh(w), tag)
}

func (c localCredentials) Rollouts(ctx context.Context, w *domain.Watch) ([]domain.NodeRollout, error) {
	return c.inner.Rollouts(ctx, c.refresh(w))
}

func (c localCredentials) RunningImage(ctx context.Context, w *domain.Watch) (string, error) {
	return c.inner.RunningImage(ctx, c.refresh(w))
}
