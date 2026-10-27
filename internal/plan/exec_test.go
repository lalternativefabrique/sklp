package plan

import (
	"context"
	"strings"
	"testing"
)

// execAllow is the allowlist used by exec tests. It is small on
// purpose: only the commands we actually call (echo, true, false,
// sh-free shell builtins via /usr/bin/X). bash_allowlisted requires
// bare basenames so we stick to ones every Linux has.
var execAllow = []string{
	"echo",
	"true",
	"false",
	"printf",
	"cat",
}

// TestExecuteHappyPath runs a Plan whose preflight is `true` and whose
// only service builds via `echo`. Verifies the report shape and that
// every step is StatusSucceeded.
func TestExecuteHappyPath(t *testing.T) {
	p := &Plan{
		SchemaVersion: SchemaVersion,
		Kind:          KindPublish,
		Preflights: []PreflightStep{
			{
				ID:           "ok",
				Commands:     []string{"true"},
				SuccessCheck: SuccessExitZero,
				OnFail:       AbortRule{Mode: AbortAll},
			},
		},
		Services: map[string]ServiceSteps{
			"web": {
				Image: "rg/web:<publish_tag>",
				Build: []string{"echo building <service>"},
				Push:  []string{"echo pushing <publish_tag>"},
			},
		},
	}
	rep, err := Execute(context.Background(), ExecConfig{
		Plan:             p,
		Repo:             t.TempDir(),
		ImpactedServices: []string{"web"},
		PublishTag:       "20260101-0000-deadbeef",
		Allowlist:        execAllow,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if rep.Phase != "apply" {
		t.Fatalf("phase = %q", rep.Phase)
	}
	if len(rep.PreflightResults) != 1 || rep.PreflightResults[0].Status != StatusPassed {
		t.Fatalf("preflight not passed: %+v", rep.PreflightResults)
	}
	if len(rep.PlanOrPublished) != 1 || rep.PlanOrPublished[0].Status != StatusSucceeded {
		t.Fatalf("service not succeeded: %+v", rep.PlanOrPublished)
	}
	if got := rep.PlanOrPublished[0].Image; got != "rg/web:20260101-0000-deadbeef" {
		t.Fatalf("image substitution wrong: %q", got)
	}
}

// TestExecuteDryRun runs preflights but records services as planned,
// not executed. Used by `sklp publish -dry-run`.
func TestExecuteDryRun(t *testing.T) {
	p := &Plan{
		SchemaVersion: SchemaVersion,
		Kind:          KindPublish,
		Preflights: []PreflightStep{
			{ID: "ok", Commands: []string{"true"}, SuccessCheck: SuccessExitZero, OnFail: AbortRule{Mode: AbortAll}},
		},
		Services: map[string]ServiceSteps{
			"web": {
				Image: "rg/web:<publish_tag>",
				Build: []string{"echo build <service>"},
				Push:  []string{"echo push <publish_tag>"},
			},
		},
	}
	rep, err := Execute(context.Background(), ExecConfig{
		Plan:             p,
		Repo:             t.TempDir(),
		ImpactedServices: []string{"web"},
		PublishTag:       "T",
		DryRun:           true,
		Allowlist:        execAllow,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if rep.Phase != "dry-run" {
		t.Fatalf("phase = %q", rep.Phase)
	}
	if rep.PlanOrPublished[0].Status != StatusPlanned {
		t.Fatalf("service not planned: %+v", rep.PlanOrPublished[0])
	}
	if !strings.Contains(rep.PlanOrPublished[0].Command, "echo build web") {
		t.Fatalf("command not substituted: %q", rep.PlanOrPublished[0].Command)
	}
}

// TestExecutePreflightAbortAll: a Mode=AbortAll preflight failure
// short-circuits the run; every impacted service is marked skipped.
func TestExecutePreflightAbortAll(t *testing.T) {
	p := &Plan{
		SchemaVersion: SchemaVersion,
		Kind:          KindPublish,
		Preflights: []PreflightStep{
			{ID: "fails", Commands: []string{"false"}, SuccessCheck: SuccessExitZero, OnFail: AbortRule{Mode: AbortAll, Message: "nope"}},
		},
		Services: map[string]ServiceSteps{
			"web": {Image: "rg/web:<publish_tag>", Build: []string{"echo b"}, Push: []string{"echo p"}},
		},
	}
	rep, _ := Execute(context.Background(), ExecConfig{
		Plan: p, Repo: t.TempDir(),
		ImpactedServices: []string{"web"},
		PublishTag:       "T",
		Allowlist:        execAllow,
	})
	if rep.PreflightResults[0].Status != StatusFailed {
		t.Fatal("preflight should be failed")
	}
	if rep.PlanOrPublished[0].Status != StatusSkipped || rep.PlanOrPublished[0].BlockedBy != "fails" {
		t.Fatalf("service should be skipped/blocked: %+v", rep.PlanOrPublished[0])
	}
}

// TestExecutePreflightAbortServices: a partial failure only skips
// the named services; other services keep running.
func TestExecutePreflightAbortServices(t *testing.T) {
	p := &Plan{
		SchemaVersion: SchemaVersion,
		Kind:          KindPublish,
		Preflights: []PreflightStep{
			{
				ID:           "audit",
				Commands:     []string{"false"},
				SuccessCheck: SuccessExitZero,
				OnFail:       AbortRule{Mode: AbortServices, AbortedServices: []string{"web"}},
			},
		},
		Services: map[string]ServiceSteps{
			"core": {Image: "rg/core:<publish_tag>", Build: []string{"echo build-core"}, Push: []string{"echo push-core"}},
			"web":  {Image: "rg/web:<publish_tag>", Build: []string{"echo build-web"}, Push: []string{"echo push-web"}},
		},
	}
	rep, _ := Execute(context.Background(), ExecConfig{
		Plan: p, Repo: t.TempDir(),
		ImpactedServices: []string{"core", "web"},
		PublishTag:       "T",
		Allowlist:        execAllow,
	})
	statusOf := map[string]StepStatus{}
	for _, e := range rep.PlanOrPublished {
		statusOf[e.Service] = e.Status
	}
	if statusOf["core"] != StatusSucceeded {
		t.Fatalf("core should succeed: %+v", rep.PlanOrPublished)
	}
	if statusOf["web"] != StatusSkipped {
		t.Fatalf("web should be skipped: %+v", rep.PlanOrPublished)
	}
}

// TestExecuteSuccessStdoutEquals: the SuccessCheck enum drives a
// pass/fail decision beyond just exit code.
func TestExecuteSuccessStdoutEquals(t *testing.T) {
	p := &Plan{
		SchemaVersion: SchemaVersion,
		Kind:          KindPublish,
		Preflights: []PreflightStep{
			{
				ID:           "expect-foo",
				Commands:     []string{"echo foo"},
				SuccessCheck: SuccessStdoutEquals,
				OnFail:       AbortRule{Mode: AbortAll, Expected: "foo"},
			},
		},
	}
	rep, _ := Execute(context.Background(), ExecConfig{
		Plan: p, Repo: t.TempDir(),
		PublishTag: "T",
		Allowlist:  execAllow,
	})
	if rep.PreflightResults[0].Status != StatusPassed {
		t.Fatalf("expected passed, got %+v", rep.PreflightResults[0])
	}

	// Wrong expected → failed.
	p.Preflights[0].OnFail.Expected = "bar"
	rep2, _ := Execute(context.Background(), ExecConfig{
		Plan: p, Repo: t.TempDir(),
		PublishTag: "T",
		Allowlist:  execAllow,
	})
	if rep2.PreflightResults[0].Status != StatusFailed {
		t.Fatalf("expected failed, got %+v", rep2.PreflightResults[0])
	}
}

// TestExecuteServiceFailureStopsDownstream: a build that exits non-zero
// stops the run; subsequent services in publish_order are skipped.
func TestExecuteServiceFailureStopsDownstream(t *testing.T) {
	p := &Plan{
		SchemaVersion: SchemaVersion,
		Kind:          KindPublish,
		Services: map[string]ServiceSteps{
			"core": {Image: "rg/core:<publish_tag>", Build: []string{"false"}, Push: []string{"echo push-core"}},
			"web":  {Image: "rg/web:<publish_tag>", Build: []string{"echo build-web"}, Push: []string{"echo push-web"}},
		},
	}
	rep, _ := Execute(context.Background(), ExecConfig{
		Plan: p, Repo: t.TempDir(),
		ImpactedServices: []string{"core", "web"},
		PublishTag:       "T",
		Allowlist:        execAllow,
	})
	statusOf := map[string]StepStatus{}
	for _, e := range rep.PlanOrPublished {
		statusOf[e.Service] = e.Status
	}
	if statusOf["core"] != StatusFailed {
		t.Fatalf("core should fail: %+v", rep.PlanOrPublished)
	}
	if statusOf["web"] != StatusSkipped {
		t.Fatalf("web should be skipped (upstream failed): %+v", rep.PlanOrPublished)
	}
}

// TestExecuteRejectsUnallowlistedAtRuntime: even though Validate is
// supposed to catch this, the executor revalidates the allowlist at
// run time as defense in depth (cache could have been written by an
// older Validate).
func TestExecuteRejectsUnallowlistedAtRuntime(t *testing.T) {
	p := &Plan{
		SchemaVersion: SchemaVersion,
		Kind:          KindPublish,
		Services: map[string]ServiceSteps{
			"web": {Image: "x", Build: []string{"curl https://evil"}, Push: []string{"echo p"}},
		},
	}
	rep, _ := Execute(context.Background(), ExecConfig{
		Plan: p, Repo: t.TempDir(),
		ImpactedServices: []string{"web"},
		PublishTag:       "T",
		Allowlist:        execAllow, // no "curl"
	})
	if rep.PlanOrPublished[0].Status != StatusFailed {
		t.Fatalf("expected failed status when allowlist denies command: %+v", rep.PlanOrPublished[0])
	}
}

// TestExecuteDiagnoseHookOnServiceFailure verifies the failure hook
// is called exactly once with the failing step's context, and that
// the returned Diagnosis is attached to the report entry.
func TestExecuteDiagnoseHookOnServiceFailure(t *testing.T) {
	var got *FailureContext
	hook := func(_ context.Context, f FailureContext) *Diagnosis {
		got = &f
		return &Diagnosis{
			RootCause:         "build script returned 1",
			LikelyExplanation: "the unit test step exits non-zero on purpose",
			SuggestedFix:      "fix the test",
		}
	}
	p := &Plan{
		SchemaVersion: SchemaVersion,
		Kind:          KindPublish,
		Services: map[string]ServiceSteps{
			"web": {Image: "x", Build: []string{"false"}, Push: []string{"echo p"}},
		},
	}
	rep, _ := Execute(context.Background(), ExecConfig{
		Plan: p, Repo: t.TempDir(),
		ImpactedServices: []string{"web"},
		PublishTag:       "T",
		Allowlist:        execAllow,
		Diagnose:         hook,
	})
	if got == nil {
		t.Fatal("diagnose hook was not called")
	}
	if got.Kind != "service" || got.StepID != "web" || got.Phase != "build" {
		t.Fatalf("failure context wrong: %+v", got)
	}
	if rep.PlanOrPublished[0].Diagnosis == nil {
		t.Fatalf("diagnosis not attached: %+v", rep.PlanOrPublished[0])
	}
	if rep.PlanOrPublished[0].Diagnosis.RootCause != "build script returned 1" {
		t.Fatalf("diagnosis content wrong: %+v", rep.PlanOrPublished[0].Diagnosis)
	}
}

// TestExecuteDiagnoseHookOnPreflightFailure: the same hook is also
// invoked when a preflight fails; the FailureContext.Kind must be
// "preflight" so the diagnoser can adapt its prompt.
func TestExecuteDiagnoseHookOnPreflightFailure(t *testing.T) {
	var got *FailureContext
	hook := func(_ context.Context, f FailureContext) *Diagnosis {
		got = &f
		return &Diagnosis{RootCause: "preflight failed"}
	}
	p := &Plan{
		SchemaVersion: SchemaVersion,
		Kind:          KindPublish,
		Preflights: []PreflightStep{
			{ID: "fail-it", Commands: []string{"false"}, SuccessCheck: SuccessExitZero, OnFail: AbortRule{Mode: AbortAll}},
		},
		Services: map[string]ServiceSteps{
			"web": {Image: "x", Build: []string{"echo b"}, Push: []string{"echo p"}},
		},
	}
	rep, _ := Execute(context.Background(), ExecConfig{
		Plan: p, Repo: t.TempDir(),
		ImpactedServices: []string{"web"},
		PublishTag:       "T",
		Allowlist:        execAllow,
		Diagnose:         hook,
	})
	if got == nil || got.Kind != "preflight" || got.StepID != "fail-it" {
		t.Fatalf("preflight failure ctx wrong: %+v", got)
	}
	if rep.PreflightResults[0].Diagnosis == nil {
		t.Fatal("preflight diagnosis not attached")
	}
}

// TestExecuteSubstitutesPlaceholders: <publish_tag> and <service>
// reach the actual exec. We verify by capturing stdout via OnCommandLine.
func TestExecuteSubstitutesPlaceholders(t *testing.T) {
	var lines []string
	p := &Plan{
		SchemaVersion: SchemaVersion,
		Kind:          KindPublish,
		Services: map[string]ServiceSteps{
			"web": {Image: "rg/<service>:<publish_tag>", Build: []string{"echo <service>-<publish_tag>"}, Push: []string{"echo done"}},
		},
	}
	rep, err := Execute(context.Background(), ExecConfig{
		Plan: p, Repo: t.TempDir(),
		ImpactedServices: []string{"web"},
		PublishTag:       "TAG42",
		Allowlist:        execAllow,
		OnCommandLine: func(stream, command, line string) {
			if stream == "stdout" {
				lines = append(lines, line)
			}
		},
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if rep.PlanOrPublished[0].Image != "rg/web:TAG42" {
		t.Fatalf("image not substituted: %q", rep.PlanOrPublished[0].Image)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "web-TAG42") {
		t.Fatalf("placeholders not substituted in exec: %q", joined)
	}
}
