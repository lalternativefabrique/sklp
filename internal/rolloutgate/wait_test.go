//go:build linux

package rolloutgate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/spacemanager/control"
)

func TestAppliedIgnoresEarlierJobRun(t *testing.T) {
	state := control.StateResponse{Services: []control.ServiceState{{
		Name:  "migrate",
		State: "done",
		Image: "reg/app/migrate:2",
		Runs:  []control.JobRunState{{Image: "reg/app/migrate:1", FinishedAt: time.Now()}},
	}}}
	if done, err := Applied(state, "migrate", "reg/app/migrate:2"); done || err != nil {
		t.Fatalf("Applied = %v, %v", done, err)
	}
}

func TestAppliedJobSucceeded(t *testing.T) {
	state := control.StateResponse{Services: []control.ServiceState{{
		Name: "migrate",
		Runs: []control.JobRunState{{Image: "m:2", FinishedAt: time.Now()}},
	}}}
	if done, err := Applied(state, "migrate", "m:2"); !done || err != nil {
		t.Fatalf("Applied = %v, %v", done, err)
	}
}

func TestAppliedJobFailed(t *testing.T) {
	state := control.StateResponse{Services: []control.ServiceState{{
		Name:  "migrate",
		State: "failed",
		Runs:  []control.JobRunState{{Image: "m:2", ExitCode: 1, Failed: true, FinishedAt: time.Now()}},
	}}}
	if _, err := Applied(state, "migrate", "m:2"); err == nil {
		t.Fatal("a failed job must stop the publish")
	}
}

func TestAppliedServiceReady(t *testing.T) {
	state := control.StateResponse{Services: []control.ServiceState{{Name: "core", State: "ready", Image: "c:2"}}}
	if done, _ := Applied(state, "core", "c:1"); done {
		t.Fatal("a service on another image is not applied")
	}
	if done, _ := Applied(state, "core", "c:2"); !done {
		t.Fatal("a ready service on the image is applied")
	}
}

func TestAppliedRejected(t *testing.T) {
	state := control.StateResponse{Services: []control.ServiceState{{Name: "core", Image: "c:1", RejectedReason: "never ready"}}}
	if _, err := Applied(state, "core", "c:2"); err == nil {
		t.Fatal("a rejected rollout must stop the publish")
	}
}

func TestPollTimesOut(t *testing.T) {
	read := func(context.Context) (control.StateResponse, error) { return control.StateResponse{}, nil }
	err := poll(context.Background(), read, "core", "c:2", 20*time.Millisecond, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "not applied") {
		t.Fatalf("poll = %v", err)
	}
}
