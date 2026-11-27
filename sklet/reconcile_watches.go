package sklet

import (
	"context"
	"strings"

	"github.com/lalternativefabrique/sklp/reconcile/imageupdate/domain"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
)

const rollsShown = 5

func (n *nodeReconcile) ListWatches(ctx context.Context) ([]control.WatchEntry, error) {
	stores, _ := n.running()
	if stores == nil {
		return []control.WatchEntry{}, nil
	}
	watches, err := stores.Watches.List(ctx)
	if err != nil {
		return nil, err
	}
	return watchEntries(ctx, control.NodeName(), watches, stores.Watches.ListRolls)
}

type rollLister func(ctx context.Context, watchID string, limit int) ([]*domain.Roll, error)

func watchEntries(ctx context.Context, node string, watches []*domain.Watch, rolls rollLister) ([]control.WatchEntry, error) {
	out := make([]control.WatchEntry, 0, len(watches))
	for _, w := range watches {
		repo, ok := strings.CutPrefix(w.StackID, node+"/")
		if !ok {
			continue
		}
		history, err := rolls(ctx, w.ID, 0)
		if err != nil {
			return nil, err
		}
		entry := control.WatchEntry{
			Name: w.Name, Repo: repo, Space: w.NodeSpace, Service: w.Service,
			Image: w.ImageRepo, TagPattern: w.TagPattern, CurrentTag: w.CurrentTag,
			Status: w.Status, HeldTag: w.HeldTag, PinnedTag: w.PinnedTag, LastError: w.LastError,
			LastCheckedAt: w.LastCheckedAt, LastRolledAt: w.LastRolledAt,
			Rolls: []control.RollEntry{},
		}
		for _, r := range history {
			if r.Result == domain.RollResultUnchanged {
				continue
			}
			entry.Rolls = append(entry.Rolls, control.RollEntry{
				FromTag: r.FromTag, ToTag: r.ToTag, Result: r.Result, Error: r.ErrorMessage,
				CreatedAt: r.CreatedAt, FinishedAt: r.FinishedAt,
			})
			if len(entry.Rolls) == rollsShown {
				break
			}
		}
		out = append(out, entry)
	}
	return out, nil
}
