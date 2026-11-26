package reconcile

import (
	"context"
	"time"

	"github.com/lalternativefabrique/sklp/reconcile/deployconfig/nodeapi"
	"github.com/lalternativefabrique/sklp/reconcile/imageupdate/rolling"
	"github.com/lalternativefabrique/sklp/reconcile/imageupdate/tags"
)

// StartImageRolling polls the registries of the watches this node keeps and
// rolls its own services onto newer tags, with no core in the loop. Rolls a
// previous run left unsettled are watched again first.
func StartImageRolling(ctx context.Context, stores *Stores, interval time.Duration) *rolling.Poller {
	node := nodeapi.NewControlRoller()
	roller := rolling.NewRoller(stores.Watches, node)
	roller.ResumePending(ctx)
	poller := rolling.NewPoller(stores.Watches, tags.NewClient(), roller, interval).WithLiveImages(node)
	poller.Start()
	return poller
}
