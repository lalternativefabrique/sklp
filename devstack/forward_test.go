package devstack

import (
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/pipeline"
)

func TestParseForwardArgs(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		stack   string
		want    forwardTarget
		wantErr string
	}{
		{"service only", []string{"stack", "postgres"}, "stack", forwardTarget{Service: "postgres"}, ""},
		{"service and port", []string{"stack", "postgres:5433"}, "stack", forwardTarget{Service: "postgres", Port: 5433}, ""},
		{"host port", []string{"stack", "postgres", "15432"}, "stack", forwardTarget{Service: "postgres", HostPort: 15432}, ""},
		{"missing service", []string{"stack"}, "", forwardTarget{}, "usage"},
		{"too many", []string{"a", "b", "1", "2"}, "", forwardTarget{}, "usage"},
		{"empty service", []string{"stack", ":5432"}, "", forwardTarget{}, "usage"},
		{"bad port", []string{"stack", "postgres:pg"}, "", forwardTarget{}, "service port"},
		{"bad host port", []string{"stack", "postgres", "70000"}, "", forwardTarget{}, "host port"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stack, got, err := parseForwardArgs(c.args)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if stack != c.stack || got != c.want {
				t.Fatalf("got (%q, %+v), want (%q, %+v)", stack, got, c.stack, c.want)
			}
		})
	}
}

func TestResolveForwardPort(t *testing.T) {
	stack := &Stack{Services: []NamedService{
		{Name: "postgres", Def: pipeline.Service{Port: 5432}},
		{Name: "core", Def: pipeline.Service{Ready: &pipeline.Ready{HTTP: "http://core:4100/health"}}},
		{Name: "worker", Def: pipeline.Service{}},
	}}
	cases := []struct {
		name    string
		target  forwardTarget
		want    int
		wantErr string
	}{
		{"declared port", forwardTarget{Service: "postgres"}, 5432, ""},
		{"explicit port wins", forwardTarget{Service: "postgres", Port: 5433}, 5433, ""},
		{"port from ready url", forwardTarget{Service: "core"}, 4100, ""},
		{"explicit port without declaration", forwardTarget{Service: "worker", Port: 9000}, 9000, ""},
		{"no port", forwardTarget{Service: "worker"}, 0, "declares no port"},
		{"unknown service", forwardTarget{Service: "redis"}, 0, "unknown service"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolveForwardPort(stack, c.target)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, c.wantErr)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("got (%d, %v), want %d", got, err, c.want)
			}
		})
	}
}
