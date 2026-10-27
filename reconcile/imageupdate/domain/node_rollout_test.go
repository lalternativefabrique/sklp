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
