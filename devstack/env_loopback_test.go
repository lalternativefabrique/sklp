package devstack

import (
	"slices"
	"testing"
)

func TestRewriteLoopbackPortsPointsAtTheOwningService(t *testing.T) {
	owners := map[int]string{5432: "postgres", 4002: "api"}
	env := []string{
		"DATABASE_URL=postgres://u:p@localhost:5432/db?sslmode=disable",
		"API_URL=http://127.0.0.1:4002/v1",
		"OTHER=http://localhost:9999/",
		"PORT=4002",
		"NOEQ",
	}
	got := rewriteLoopbackPorts(env, "api", owners)
	want := []string{
		"DATABASE_URL=postgres://u:p@postgres:5432/db?sslmode=disable",
		"API_URL=http://127.0.0.1:4002/v1",
		"OTHER=http://localhost:9999/",
		"PORT=4002",
		"NOEQ",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("rewritten env = %v\nwant %v", got, want)
	}
}

func TestRewriteLoopbackPortsIsANoopWithoutPorts(t *testing.T) {
	env := []string{"A=localhost:5432"}
	if got := rewriteLoopbackPorts(env, "x", nil); !slices.Equal(got, env) {
		t.Fatalf("got %v", got)
	}
}
