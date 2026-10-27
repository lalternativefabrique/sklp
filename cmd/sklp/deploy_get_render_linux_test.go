package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/spacemanager/control"
)

func TestWriteRolloutsHidesASucceededRollout(t *testing.T) {
	var out bytes.Buffer
	writeRollouts(&out, []control.ServiceState{{
		Name: "web",
		Rollouts: []control.RolloutState{{
			ID: "r1", ToImage: "web:v2", Result: control.RolloutSucceeded,
			StartedAt: time.Now().Add(-time.Hour),
		}},
	}})
	if out.Len() != 0 {
		t.Fatalf("printed a succeeded rollout:\n%s", out.String())
	}
}

func TestWriteRolloutsShowsAnInFlightAndAFailedOne(t *testing.T) {
	var out bytes.Buffer
	writeRollouts(&out, []control.ServiceState{
		{
			Name: "core",
			Rollouts: []control.RolloutState{
				{ID: "r1", ToImage: "core:v2", Result: control.RolloutRolledBack, Reason: "exited 1 before ready", StartedAt: time.Now().Add(-90 * time.Second)},
				{ID: "r2", ToImage: "core:v3", Result: control.RolloutSoaking, StartedAt: time.Now().Add(-30 * time.Second)},
			},
		},
		{Name: "db", Rollouts: []control.RolloutState{{ID: "r3", ToImage: "db:v9", Result: control.RolloutSucceeded}}},
	})
	got := out.String()
	for _, want := range []string{"ROLLOUTS", "rolled_back", "exited 1 before ready", "core:v3", "soaking"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "db:v9") {
		t.Errorf("listed a succeeded rollout:\n%s", got)
	}
}

// A frozen service is reported even with no rollout left to show: the refusal
// is the whole reason its applies are doing nothing.
func TestWriteRolloutsReportsAFrozenService(t *testing.T) {
	var out bytes.Buffer
	writeRollouts(&out, []control.ServiceState{{
		Name:           "web",
		RejectedReason: "exited 1 before ready",
	}})
	got := out.String()
	if !strings.Contains(got, "FROZEN") || !strings.Contains(got, "web") {
		t.Fatalf("frozen service not reported:\n%s", got)
	}
}

func TestInstanceStatusMarksAHeldJob(t *testing.T) {
	if got := instanceStatus(control.InstanceState{State: "failed", Held: true}); got != "failed (held)" {
		t.Errorf("got %q, want the held marker", got)
	}
	if got := instanceStatus(control.InstanceState{State: "ready"}); got != "ready" {
		t.Errorf("got %q, want the bare state", got)
	}
}

// An older node reports none of these counters; the columns must stay empty
// rather than claim a zero-second uptime or a restart that never happened.
func TestUptimeAndRestartsOfAnOlderNode(t *testing.T) {
	var zero control.InstanceState
	if got := uptimeOf(zero); got != "-" {
		t.Errorf("uptime = %q, want %q", got, "-")
	}
	if got := restartsOf(zero); got != "-" {
		t.Errorf("restarts = %q, want %q", got, "-")
	}
}

func TestRestartsOfShowsTheBackoff(t *testing.T) {
	got := restartsOf(control.InstanceState{Restarts: 7, BackoffMS: 30000})
	if !strings.Contains(got, "7") || !strings.Contains(got, "30s") {
		t.Fatalf("got %q, want the count and its backoff", got)
	}
}

func TestUptimeOfAnInstanceAwaitingRestart(t *testing.T) {
	at := time.Now().Add(8 * time.Second)
	got := uptimeOf(control.InstanceState{RestartAt: &at})
	if !strings.HasPrefix(got, "restart in") {
		t.Fatalf("got %q, want the pending restart", got)
	}
}

// A restart whose due time has passed is late, not "in 0s".
func TestUptimeOfAnOverdueRestart(t *testing.T) {
	at := time.Now().Add(-3 * time.Minute)
	if got := uptimeOf(control.InstanceState{RestartAt: &at}); got != "restart due" {
		t.Fatalf("got %q, want %q", got, "restart due")
	}
}

// A backoff with no completed run is a service that cannot start at all —
// the one case where a zero restart count still means something.
func TestRestartsOfAFailingFirstLaunch(t *testing.T) {
	got := restartsOf(control.InstanceState{Restarts: 0, BackoffMS: 2000})
	if !strings.Contains(got, "backoff") {
		t.Fatalf("got %q, want the backoff to survive a zero restart count", got)
	}
}

// TestWriteRolloutsForgetsFailuresBeforeTheLastSuccess: a service that rolled
// back twice and then rolled cleanly is fine. Keeping the two failures on
// screen leaves a fixed afternoon looking broken.
func TestWriteRolloutsForgetsFailuresBeforeTheLastSuccess(t *testing.T) {
	var out bytes.Buffer
	writeRollouts(&out, []control.ServiceState{{
		Name: "ui",
		Rollouts: []control.RolloutState{
			{ID: "r1", ToImage: "ui:v1", Result: control.RolloutRolledBack, Reason: "port 80"},
			{ID: "r2", ToImage: "ui:v2", Result: control.RolloutRolledBack, Reason: "probe"},
			{ID: "r3", ToImage: "ui:v3", Result: control.RolloutSucceeded},
		},
	}})
	if out.Len() != 0 {
		t.Fatalf("failures before a clean roll were printed:\n%s", out.String())
	}
}

func TestWriteRolloutsKeepsFailuresAfterTheLastSuccess(t *testing.T) {
	var out bytes.Buffer
	writeRollouts(&out, []control.ServiceState{{
		Name: "ui",
		Rollouts: []control.RolloutState{
			{ID: "r1", ToImage: "ui:v1", Result: control.RolloutSucceeded},
			{ID: "r2", ToImage: "ui:v2", Result: control.RolloutRolledBack, Reason: "probe timed out"},
		},
	}})
	got := out.String()
	if !strings.Contains(got, "ui:v2") || !strings.Contains(got, "probe timed out") {
		t.Fatalf("a failure after the last success was dropped:\n%s", got)
	}
	if strings.Contains(got, "ui:v1") {
		t.Fatalf("the successful roll was listed:\n%s", got)
	}
}
