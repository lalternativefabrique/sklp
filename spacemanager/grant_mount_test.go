package spacemanager

import (
	"testing"

	"github.com/lalternativefabrique/sklp/spacemanager/grants"
)

func TestAGrantMountsInItsServiceOnlyWhateverTheInstanceID(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := grants.Put(grants.Grant{Space: "playground", Service: "web", Owner: "prod", Scope: grants.Read}); err != nil {
		t.Fatal(err)
	}
	a := &Agent{}
	l := &runcLauncher{space: "playground"}
	cases := map[string]struct{ service, id, want string }{
		"replica of the granted service":  {"web", "web-0", grants.Dir("playground", "web")},
		"rollout generation":              {"web", "web-v3-0", grants.Dir("playground", "web")},
		"a service named like a rollout":  {"web-v2", "web-v2-0", ""},
		"another service of the space":    {"api", "api-0", ""},
		"a spec that never got a service": {"", "web-0", ""},
	}
	for name, c := range cases {
		inst := a.newInstanceLocked(c.service, Service{Name: c.service}, c.id)
		if c.service == "" {
			inst.svc.Service = ""
		}
		if got := l.controlSocketDir(inst.svc.Service); got != c.want {
			t.Errorf("%s: mount %q, want %q", name, got, c.want)
		}
	}
}
