package space

import (
	"encoding/json"
	"reflect"
	"testing"
)

func toMap(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

// nsByType returns the namespace entry of the given type, or nil.
func nsByType(spec map[string]any, typ string) map[string]any {
	for _, n := range spec["linux"].(map[string]any)["namespaces"].([]any) {
		nn := n.(map[string]any)
		if nn["type"] == typ {
			return nn
		}
	}
	return nil
}

func TestBuildRunnerSpec_LeaderCreatesNamespaces(t *testing.T) {
	// join == nil → leader: creates its own net/user ns (no path), keeps
	// uid/gid mappings. This must be byte-identical to buildSpec.
	cfg := Config{RepoPath: "/repo", TaskName: "ci"}
	leader := toMap(t, buildRunnerSpec("/rootfs", "", cfg, 1000, 1000, nil))
	plain := toMap(t, buildSpec("/rootfs", "", cfg, 1000, 1000))
	if !reflect.DeepEqual(leader, plain) {
		t.Fatal("runner spec with nil join must equal the single-container buildSpec")
	}
	net := nsByType(leader, "network")
	if net == nil {
		t.Fatal("network namespace missing")
	}
	if _, hasPath := net["path"]; hasPath {
		t.Error("leader network ns must NOT carry a path (it creates a fresh one)")
	}
	if _, ok := leader["linux"].(map[string]any)["uidMappings"]; !ok {
		t.Error("leader must keep uidMappings")
	}
}

func TestBuildRunnerSpec_JoinerReusesLeaderNamespaces(t *testing.T) {
	join := &joinNamespaces{
		NetnsPath:  "/proc/4242/ns/net",
		UsernsPath: "/proc/4242/ns/user",
	}
	spec := toMap(t, buildRunnerSpec("/rootfs", "", Config{RepoPath: "/repo"}, 1000, 1000, join))
	net := nsByType(spec, "network")
	if net == nil || net["path"] != "/proc/4242/ns/net" {
		t.Errorf("joiner net ns path = %v, want /proc/4242/ns/net", net)
	}
	user := nsByType(spec, "user")
	if user == nil || user["path"] != "/proc/4242/ns/user" {
		t.Errorf("joiner user ns path = %v, want /proc/4242/ns/user", user)
	}
	// The kernel rejects uid/gid remapping on a joined user ns.
	linux := spec["linux"].(map[string]any)
	if _, ok := linux["uidMappings"]; ok {
		t.Error("joiner must drop uidMappings")
	}
	if _, ok := linux["gidMappings"]; ok {
		t.Error("joiner must drop gidMappings")
	}
}

func TestBuildServiceSpec_LeaderVsJoiner(t *testing.T) {
	svc := Service{Name: "db", Image: "postgres:16-alpine"}

	leader := toMap(t, buildServiceSpec("/rootfs", svc, []string{"docker-entrypoint.sh"}, []string{"postgres"}, []string{"POSTGRES_DB=app"}, "", 1000, 1000, nil))
	if n := nsByType(leader, "network"); n == nil {
		t.Fatal("leader service missing network ns")
	} else if _, hasPath := n["path"]; hasPath {
		t.Error("leader service network ns must not carry a path")
	}
	if _, ok := leader["linux"].(map[string]any)["uidMappings"]; !ok {
		t.Error("leader service must keep uidMappings (postgres chowns its data dir)")
	}

	join := &joinNamespaces{NetnsPath: "/proc/7/ns/net", UsernsPath: "/proc/7/ns/user"}
	joiner := toMap(t, buildServiceSpec("/rootfs", svc, nil, nil, nil, "", 1000, 1000, join))
	if n := nsByType(joiner, "network"); n == nil || n["path"] != "/proc/7/ns/net" {
		t.Errorf("joiner service net path = %v", n)
	}
	if _, ok := joiner["linux"].(map[string]any)["gidMappings"]; ok {
		t.Error("joiner service must drop gidMappings")
	}
}

func TestBuildServiceSpec_NoWorkspaceMount(t *testing.T) {
	spec := toMap(t, buildServiceSpec("/rootfs", Service{Name: "db", Image: "x"}, []string{"x"}, nil, nil, "", 1000, 1000, nil))
	for _, m := range spec["mounts"].([]any) {
		if m.(map[string]any)["destination"] == "/workspace" {
			t.Error("a service must NOT bind-mount /workspace")
		}
	}
}

func TestBuildServiceSpec_EnvLayering(t *testing.T) {
	svc := Service{Name: "db", Image: "x", Env: []string{"POSTGRES_DB=app"}}
	spec := toMap(t, buildServiceSpec("/rootfs", svc, []string{"x"}, nil, []string{"LANG=C", "POSTGRES_DB=base"}, "", 1000, 1000, nil))
	env := spec["process"].(map[string]any)["env"].([]any)
	// PATH default first, then image env, then service env (wins on dup).
	if env[len(env)-1] != "POSTGRES_DB=app" {
		t.Errorf("service env must be appended last (so it wins): %v", env)
	}
}

func TestServiceArgs_DockerSemantics(t *testing.T) {
	// Command overrides the image CMD but KEEPS the entrypoint, appended
	// (Docker): nats entrypoint [nats-server] + command [-js] → nats-server -js.
	if got := serviceArgs([]string{"-js", "-sd", "/data"}, []string{"nats-server"}, []string{"cmd"}, false); !reflect.DeepEqual(got, []string{"nats-server", "-js", "-sd", "/data"}) {
		t.Errorf("command must be appended to entrypoint: %v", got)
	}
	// No command → entrypoint + image cmd (the image default).
	if got := serviceArgs(nil, []string{"entry"}, []string{"cmd"}, false); !reflect.DeepEqual(got, []string{"entry", "cmd"}) {
		t.Errorf("no command → entrypoint+cmd: %v", got)
	}
	// IgnoreEntrypoint (dev run:): command runs verbatim, entrypoint discarded.
	if got := serviceArgs([]string{"sh", "-c", "x"}, []string{"sklp"}, []string{"cmd"}, true); !reflect.DeepEqual(got, []string{"sh", "-c", "x"}) {
		t.Errorf("IgnoreEntrypoint must run command verbatim: %v", got)
	}
}

func TestNsPathsFor(t *testing.T) {
	j := nsPathsFor(12345)
	if j.NetnsPath != "/proc/12345/ns/net" || j.UsernsPath != "/proc/12345/ns/user" {
		t.Errorf("nsPathsFor = %+v", j)
	}
}
