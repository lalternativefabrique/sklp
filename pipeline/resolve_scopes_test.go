package pipeline

import (
	"reflect"
	"testing"
)

// TestImpactedStepServices covers the modern contract shape (scopes: +
// publish steps, no service_map). Regression guard for the bug where a
// nested path like apps/web/src/pages/x.tsx failed to impact `web`
// because the resolver only knew about service_map.
func TestImpactedStepServices(t *testing.T) {
	scopes := map[string][]string{
		"core":    {"services/api/**", "Dockerfile", "go.work"},
		"web":     {"apps/web/**", "packages/**", "Dockerfile.web"},
		"migrate": {"apps/migrations/**"},
	}
	steps := []contractStep{
		{Name: "core-image", Kind: "publish", Scope: scopeRef{"core"}},
		{Name: "web-image", Kind: "publish", Scope: scopeRef{"web"}},
		{Name: "migrate-image", Kind: "publish", Scope: scopeRef{"migrate"}},
	}

	cases := []struct {
		name    string
		changed []string
		want    []string
	}{
		{"nested web path (the reported bug)", []string{"apps/web/src/pages/stack-issue-detail.tsx"}, []string{"web"}},
		{"direct web child", []string{"apps/web/App.tsx"}, []string{"web"}},
		{"core only", []string{"services/api/issue/api.go"}, []string{"core"}},
		{"migrate only", []string{"apps/migrations/000042_x.up.sql"}, []string{"migrate"}},
		{"core + web split", []string{"services/api/issue/api.go", "apps/web/src/x.tsx"}, []string{"core", "web"}},
		{"shared packages hits web", []string{"packages/ui/x.ts"}, []string{"web"}},
		{"root file out of scope", []string{"README.md"}, []string{}},
	}
	for _, c := range cases {
		got, err := impactedStepServices(steps, scopes, c.changed)
		if err != nil {
			t.Errorf("%s: unexpected error: %v", c.name, err)
			continue
		}
		if len(got) == 0 && len(c.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// TestImpactedStepServices_ScopeRefScalar verifies the scalar scope form
// (scope: web) parses like the list form (scope: [web]).
func TestImpactedStepServices_ScopeRefScalar(t *testing.T) {
	scopes := map[string][]string{"web": {"apps/web/**"}}
	steps := []contractStep{
		{Name: "web-image", Kind: "publish", Scope: scopeRef{"web"}},
		// a non-publish step must never be reported as a service
		{Name: "lint", Kind: "", Scope: scopeRef{"web"}},
	}
	got, err := impactedStepServices(steps, scopes, []string{"apps/web/src/x.tsx"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"web"}) {
		t.Errorf("got %v, want [web]", got)
	}
}
