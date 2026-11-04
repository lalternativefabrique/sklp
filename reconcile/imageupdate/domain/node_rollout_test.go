package domain

import (
	"testing"
	"time"
)

func TestMatchRolloutPicksLatestForTheImage(t *testing.T) {
	now := time.Now()
	rollouts := []NodeRollout{
		{Service: "web", ToImage: "r/web:b", Result: NodeRolloutRolledBack, StartedAt: now.Add(-10 * time.Minute)},
		{Service: "web", ToImage: "r/web:b", Result: NodeRolloutSoaking, StartedAt: now},
		{Service: "core", ToImage: "r/web:b", Result: NodeRolloutSucceeded, StartedAt: now},
	}
	got := MatchRollout(rollouts, "web", "r/web:b", now.Add(-time.Minute))
	if got == nil || got.Result != NodeRolloutSoaking {
		t.Fatalf("got %+v, want the latest web rollout for the image", got)
	}
	if MatchRollout(rollouts, "web", "r/web:c", now) != nil {
		t.Fatal("matched a rollout for another image")
	}
	if MatchRollout(rollouts[:1], "web", "r/web:b", now) != nil {
		t.Fatal("matched a rollout older than the roll")
	}
}

func TestMatchRolloutExcludingSkipsPriorRollouts(t *testing.T) {
	now := time.Now()
	rollouts := []NodeRollout{
		{ID: "core-r11", Service: "core", ToImage: "r/core:b", Result: NodeRolloutRolledBack, StartedAt: now.Add(-90 * time.Second), FinishedAt: now.Add(-80 * time.Second)},
	}
	prior := map[string]bool{"core-r11": true}
	if got := MatchRolloutExcluding(rollouts, "core", "r/core:b", now, prior); got != nil {
		t.Fatalf("matched a rollout the node listed before the roll: %+v", got)
	}
	rollouts = append(rollouts, NodeRollout{ID: "core-r13", Service: "core", ToImage: "r/core:b", Result: NodeRolloutRolling, StartedAt: now})
	if got := MatchRolloutExcluding(rollouts, "core", "r/core:b", now, prior); got == nil || got.ID != "core-r13" {
		t.Fatalf("got %+v, want the rollout started after the roll", got)
	}
}

// TestMatchRolloutReadsTheFirstSettledRollout: partage-core 2026-11-04. The
// node re-rolled the image a roll had just landed, once per sibling apply,
// each starting as the previous one ended. Following the latest record kept
// the roll unsettled for as long as the chain lasted.
func TestMatchRolloutReadsTheFirstSettledRollout(t *testing.T) {
	now := time.Now()
	rollouts := []NodeRollout{
		{ID: "core-r1", Service: "core", ToImage: "r/core:b", Result: NodeRolloutSucceeded, StartedAt: now.Add(-6 * time.Minute), FinishedAt: now.Add(-3 * time.Minute)},
		{ID: "core-r2", Service: "core", ToImage: "r/core:b", Result: NodeRolloutSucceeded, StartedAt: now.Add(-3 * time.Minute), FinishedAt: now},
		{ID: "core-r3", Service: "core", ToImage: "r/core:b", Result: NodeRolloutSoaking, StartedAt: now},
	}
	if got := MatchRollout(rollouts, "core", "r/core:b", now.Add(-7*time.Minute)); got == nil || got.ID != "core-r1" {
		t.Fatalf("got %+v, want the first rollout to settle", got)
	}
	superseded := []NodeRollout{
		{ID: "core-r1", Service: "core", ToImage: "r/core:b", Result: NodeRolloutSuperseded, StartedAt: now.Add(-6 * time.Minute), FinishedAt: now.Add(-5 * time.Minute)},
		{ID: "core-r2", Service: "core", ToImage: "r/core:b", Result: NodeRolloutRolledBack, StartedAt: now.Add(-5 * time.Minute), FinishedAt: now.Add(-2 * time.Minute)},
	}
	if got := MatchRollout(superseded, "core", "r/core:b", now.Add(-7*time.Minute)); got == nil || got.ID != "core-r2" {
		t.Fatalf("got %+v, want the replacement of a superseded rollout", got)
	}
}
