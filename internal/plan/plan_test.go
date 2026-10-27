package plan

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestPlanRoundTrip ensures every field of a fully-populated Plan
// survives JSON marshal → unmarshal unchanged. Cache integrity
// depends on this: a Plan persisted today must be byte-identical
// (modulo whitespace) when loaded tomorrow.
func TestPlanRoundTrip(t *testing.T) {
	original := Plan{
		SchemaVersion:   SchemaVersion,
		Kind:            KindPublish,
		PublishTagShell: `printf "%s-%s" "$(date +%Y%m%d-%H%M)" "$(git rev-parse --short HEAD)"`,
		Preflights: []PreflightStep{
			{
				ID:           "check-main-branch",
				Description:  "must run from main",
				Commands:     []string{"git branch --show-current"},
				SuccessCheck: SuccessStdoutEquals,
				OnFail: AbortRule{
					Mode:     AbortAll,
					Expected: "main",
					Message:  "not on main",
				},
			},
			{
				ID:           "pnpm-audit-prod",
				Commands:     []string{"pnpm audit --audit-level=high --prod"},
				SuccessCheck: SuccessExitZero,
				OnFail: AbortRule{
					Mode:            AbortServices,
					AbortedServices: []string{"web"},
					Message:         "audit failed",
				},
			},
		},
		Services: map[string]ServiceSteps{
			"web": {
				Image: "rg.example/web:<publish_tag>",
				Build: []string{"make docker-build-web IMAGE_TAG=<publish_tag>"},
				Scan:  []string{"trivy image --severity HIGH,CRITICAL rg.example/web:<publish_tag>"},
				Push:  []string{"docker push rg.example/web:<publish_tag>"},
			},
		},
		ServiceMap: map[string][]string{
			"web": {"apps/web/", "Dockerfile.web"},
		},
		ExpansionRules: []ExpansionRule{
			{WhenService: "shared", Publish: []string{"core", "web"}},
		},
		Deps: map[string][]string{
			"web": {"core"},
		},
	}

	b, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded Plan
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if !reflect.DeepEqual(original, decoded) {
		t.Fatalf("round-trip mismatch:\noriginal=%+v\ndecoded =%+v", original, decoded)
	}
}

// TestNeedsAgentFlag verifies the escape hatch survives a round trip
// and that it can be set without filling the rest of the plan — the
// compiler may emit a NeedsAgent-only plan when the YAML is ambiguous.
func TestNeedsAgentFlag(t *testing.T) {
	p := Plan{
		SchemaVersion:    SchemaVersion,
		Kind:             KindPublish,
		NeedsAgent:       true,
		NeedsAgentReason: "contract uses conditional `when_impacted_services_includes` we do not model",
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Plan
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.NeedsAgent || got.NeedsAgentReason == "" {
		t.Fatalf("needs_agent flag lost: %+v", got)
	}
}

// TestSchemaVersionConstant pins the version. Bumping it should be a
// deliberate act: this test forces a code review when someone changes
// the constant by failing CI until the test is updated alongside.
func TestSchemaVersionConstant(t *testing.T) {
	const pinnedVersion = 10
	if SchemaVersion != pinnedVersion {
		t.Fatalf("SchemaVersion changed to %d — bump pinnedVersion in this test deliberately, then update the cache_key derivation if needed", SchemaVersion)
	}
}
