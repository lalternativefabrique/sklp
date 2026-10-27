package spacemanager

import "testing"

func TestValidRoutePath(t *testing.T) {
	for _, ok := range []string{"", "/", "/api/v1", "/api/auth", "/a-b_c.d~e/%20/x:y@z"} {
		if !validRoutePath(ok) {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"api", "/a`) || Host(`evil.example`", "/a\"\n  evil: x", "/a\\b", "/a b", "/a\tb"} {
		if validRoutePath(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestValidateResolvedRouteRejectsInjection(t *testing.T) {
	if err := ValidateResolvedRoute(ResolvedRoute{Service: "web", Host: "app.example.com", Path: "/api"}); err != nil {
		t.Fatalf("valid route rejected: %v", err)
	}
	if err := ValidateResolvedRoute(ResolvedRoute{Service: "web", Host: "app.example.com", Path: "/x`) || Host(`victim.example.com"}); err == nil {
		t.Fatal("backtick injection in path accepted")
	}
	if err := ValidateResolvedRoute(ResolvedRoute{Service: "web", Host: "a`b.example.com"}); err == nil {
		t.Fatal("invalid host accepted")
	}
}
