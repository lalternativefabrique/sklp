package domain

import "time"

// Node-side rollout results, as the sklp control API serves them on
// GET /v1/state (spacemanager.RolloutRecord).
const (
	NodeRolloutRolling    = "rolling"
	NodeRolloutSoaking    = "soaking"
	NodeRolloutSucceeded  = "succeeded"
	NodeRolloutRolledBack = "rolled_back"
	NodeRolloutSuperseded = "superseded"
)

// NodeRollout is one rolling update of a service as the node recorded it.
type NodeRollout struct {
	ID         string
	Service    string
	FromImage  string
	ToImage    string
	Result     string
	Reason     string
	ExitCode   int
	LogTail    []string
	StartedAt  time.Time
	FinishedAt time.Time
}

// Terminal reports whether the node is done with this rollout.
func (r NodeRollout) Terminal() bool {
	switch r.Result {
	case NodeRolloutRolling, NodeRolloutSoaking:
		return false
	}
	return true
}

// MatchRollout picks, among a node's rollouts, the one a roll started: same
// service, same target image, and not older than the roll itself (allowing
// for clock skew between core and node). The first such record to settle is
// the roll's outcome; later ones are re-rolls of the same image, started by a
// sibling's apply, and following them delays the outcome by one soak each,
// past any observation window. While none has settled, the latest is the one
// still worth watching.
func MatchRollout(rollouts []NodeRollout, service, image string, notBefore time.Time) *NodeRollout {
	return MatchRolloutExcluding(rollouts, service, image, notBefore, nil)
}

// MatchRolloutExcluding is MatchRollout ignoring the rollouts the node already
// listed before the roll was posted. The skew window alone let a re-roll of a
// tag the node had just rolled back read that earlier outcome as its own.
func MatchRolloutExcluding(rollouts []NodeRollout, service, image string, notBefore time.Time, prior map[string]bool) *NodeRollout {
	var settled, latest *NodeRollout
	for i := range rollouts {
		r := &rollouts[i]
		if r.Service != service || r.ToImage != image || prior[r.ID] {
			continue
		}
		if r.StartedAt.Before(notBefore.Add(-2 * time.Minute)) {
			continue
		}
		if r.Terminal() && r.Result != NodeRolloutSuperseded && (settled == nil || r.StartedAt.Before(settled.StartedAt)) {
			settled = r
		}
		if latest == nil || r.StartedAt.After(latest.StartedAt) {
			latest = r
		}
	}
	if settled != nil {
		return settled
	}
	return latest
}
