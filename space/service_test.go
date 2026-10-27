package space

import (
	"strings"
	"testing"
)

func names(svcs []Service) []string {
	out := make([]string, len(svcs))
	for i, s := range svcs {
		out[i] = s.Name
	}
	return out
}

// indexOf returns the position of name in the ordered slice, or -1.
func indexOf(ordered []Service, name string) int {
	for i, s := range ordered {
		if s.Name == name {
			return i
		}
	}
	return -1
}

func TestOrderServices_DependencyFirst(t *testing.T) {
	svcs := []Service{
		{Name: "api", DependsOn: []string{"db"}},
		{Name: "db"},
		{Name: "migrate", DependsOn: []string{"db"}, Oneshot: true},
	}
	ordered, err := orderServices(svcs)
	if err != nil {
		t.Fatalf("orderServices: %v", err)
	}
	if got := indexOf(ordered, "db"); got >= indexOf(ordered, "api") {
		t.Errorf("db (%d) must precede api (%d): %v", got, indexOf(ordered, "api"), names(ordered))
	}
	if got := indexOf(ordered, "db"); got >= indexOf(ordered, "migrate") {
		t.Errorf("db must precede migrate: %v", names(ordered))
	}
}

func TestOrderServices_IndependentLexicographic(t *testing.T) {
	ordered, err := orderServices([]Service{{Name: "zeta"}, {Name: "alpha"}, {Name: "mid"}})
	if err != nil {
		t.Fatalf("orderServices: %v", err)
	}
	want := []string{"alpha", "mid", "zeta"}
	for i, w := range want {
		if ordered[i].Name != w {
			t.Fatalf("order = %v, want %v", names(ordered), want)
		}
	}
}

func TestOrderServices_Cycle(t *testing.T) {
	_, err := orderServices([]Service{
		{Name: "a", DependsOn: []string{"b"}},
		{Name: "b", DependsOn: []string{"a"}},
	})
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected cycle error, got %v", err)
	}
}

func TestOrderServices_SelfCycle(t *testing.T) {
	_, err := orderServices([]Service{{Name: "a", DependsOn: []string{"a"}}})
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected self cycle error, got %v", err)
	}
}

func TestOrderServices_UnknownDependency(t *testing.T) {
	_, err := orderServices([]Service{{Name: "a", DependsOn: []string{"ghost"}}})
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("expected unknown-dependency error, got %v", err)
	}
}

func TestOrderServices_Duplicate(t *testing.T) {
	_, err := orderServices([]Service{{Name: "a"}, {Name: "a"}})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected duplicate error, got %v", err)
	}
}

func TestOrderServices_TransitiveChain(t *testing.T) {
	ordered, err := orderServices([]Service{
		{Name: "c", DependsOn: []string{"b"}},
		{Name: "b", DependsOn: []string{"a"}},
		{Name: "a"},
	})
	if err != nil {
		t.Fatalf("orderServices: %v", err)
	}
	want := []string{"a", "b", "c"}
	for i, w := range want {
		if ordered[i].Name != w {
			t.Fatalf("transitive order = %v, want %v", names(ordered), want)
		}
	}
}

func TestServiceFromSchema_Mapping(t *testing.T) {
	ready := &ServiceReady{TCP: "127.0.0.1:5432", Timeout: "10s"}
	svc := ServiceFromSchema(
		"db", "postgres:16-alpine", "",
		[]string{"postgres", "-c", "listen_addresses=127.0.0.1"},
		[]string{"POSTGRES_DB=app"},
		[]string{"net"},
		false, ready,
		[]string{"5432:5432"}, []string{"pg:/var/lib/postgresql/data"}, "bridge",
	)
	if svc.Name != "db" || svc.Image != "postgres:16-alpine" {
		t.Errorf("name/image mismapped: %+v", svc)
	}
	if len(svc.Command) != 3 || svc.Command[0] != "postgres" {
		t.Errorf("command mismapped: %v", svc.Command)
	}
	if len(svc.Env) != 1 || svc.Env[0] != "POSTGRES_DB=app" {
		t.Errorf("env mismapped: %v", svc.Env)
	}
	if len(svc.DependsOn) != 1 || svc.DependsOn[0] != "net" {
		t.Errorf("dependsOn mismapped: %v", svc.DependsOn)
	}
	if svc.Ready == nil || svc.Ready.TCP != "127.0.0.1:5432" || svc.Ready.Timeout != "10s" {
		t.Errorf("ready mismapped: %+v", svc.Ready)
	}
	if len(svc.Ports) != 1 || len(svc.Volumes) != 1 || svc.Network != "bridge" {
		t.Errorf("ports/volumes/network mismapped: %+v", svc)
	}
	if svc.Run != "" {
		t.Errorf("run should be empty for an image service: %q", svc.Run)
	}
}

func TestServiceFromSchema_Run(t *testing.T) {
	svc := ServiceFromSchema(
		"migrate", "", "migrate -path apps/migrations -database $DATABASE_URL up",
		nil, []string{"DATABASE_URL=postgres://..."}, []string{"db"},
		true, nil, nil, nil, "",
	)
	if svc.Image != "" || svc.Run == "" {
		t.Errorf("run service mismapped: image=%q run=%q", svc.Image, svc.Run)
	}
	if !svc.Oneshot {
		t.Error("oneshot lost")
	}
}
