package space

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// mountByDest returns the mount entry with the given destination, or nil.
func mountByDest(spec map[string]any, dest string) map[string]any {
	for _, m := range spec["mounts"].([]any) {
		mm := m.(map[string]any)
		if mm["destination"] == dest {
			return mm
		}
	}
	return nil
}

// TestApplyJoin_PrivateNetns is the core of the space-deploy wiring: a join
// with PrivateNetns set must JOIN the user ns by path while keeping a fresh
// (pathless) network ns, and still drop the uid/gid mappings a joined user ns
// forbids.
func TestApplyJoin_PrivateNetns(t *testing.T) {
	join := &joinNamespaces{
		UsernsPath:   "/proc/99/ns/user",
		NetnsPath:    "/proc/99/ns/net", // must be IGNORED in private-netns mode
		PrivateNetns: true,
	}
	spec := toMap(t, buildServiceSpec("/rootfs", Service{Name: "db", Image: "x"},
		[]string{"x"}, nil, nil, "", 1000, 1000, join))

	user := nsByType(spec, "user")
	if user == nil || user["path"] != "/proc/99/ns/user" {
		t.Fatalf("space service must JOIN the space user ns by path, got %v", user)
	}
	net := nsByType(spec, "network")
	if net == nil {
		t.Fatal("network namespace missing")
	}
	if _, hasPath := net["path"]; hasPath {
		t.Errorf("space service must keep a PRIVATE (pathless) net ns, got %v", net)
	}
	linux := spec["linux"].(map[string]any)
	if _, ok := linux["uidMappings"]; ok {
		t.Error("joined user ns must drop uidMappings")
	}
	if _, ok := linux["gidMappings"]; ok {
		t.Error("joined user ns must drop gidMappings")
	}
}

// TestApplyJoin_SharedNetnsUnchanged guards the CI path: a classic join (no
// PrivateNetns) still joins BOTH the user and net namespaces by path, exactly
// as orchestrate.go relies on. This is the non-regression assertion for the
// additive change.
func TestApplyJoin_SharedNetnsUnchanged(t *testing.T) {
	join := &joinNamespaces{UsernsPath: "/proc/7/ns/user", NetnsPath: "/proc/7/ns/net"}
	spec := toMap(t, buildServiceSpec("/rootfs", Service{Name: "db", Image: "x"},
		[]string{"x"}, nil, nil, "", 1000, 1000, join))
	if n := nsByType(spec, "network"); n == nil || n["path"] != "/proc/7/ns/net" {
		t.Errorf("classic join must keep the shared net ns path, got %v", n)
	}
	if u := nsByType(spec, "user"); u == nil || u["path"] != "/proc/7/ns/user" {
		t.Errorf("classic join must keep the shared user ns path, got %v", u)
	}
}

// TestWriteServiceBundleNet_ResolvConfMount asserts the generated config.json
// for a space service bind-mounts the space resolv.conf read-only at
// /etc/resolv.conf, and that an empty resolvConf adds no such mount.
func TestWriteServiceBundleNet_ResolvConfMount(t *testing.T) {
	bundle := t.TempDir()
	join := &joinNamespaces{UsernsPath: "/proc/1/ns/user", PrivateNetns: true}
	if err := writeServiceBundleNet(bundle, "/rootfs", Service{Name: "api", Image: "x"},
		[]string{"x"}, nil, nil, "", join, "/tmp/spacenet-resolv.conf", "", nil, false); err != nil {
		t.Fatalf("writeServiceBundleNet: %v", err)
	}
	spec := readBundleSpec(t, bundle)

	m := mountByDest(spec, "/etc/resolv.conf")
	if m == nil {
		t.Fatal("space service must bind-mount /etc/resolv.conf")
	}
	if m["source"] != "/tmp/spacenet-resolv.conf" || m["type"] != "bind" {
		t.Errorf("resolv.conf mount = %v, want bind of /tmp/spacenet-resolv.conf", m)
	}
	opts := m["options"].([]any)
	foundRO := false
	for _, o := range opts {
		if o == "ro" {
			foundRO = true
		}
	}
	if !foundRO {
		t.Errorf("resolv.conf mount must be read-only, opts = %v", opts)
	}

	// Empty resolvConf → no /etc/resolv.conf mount (CI shared-netns path).
	bundle2 := t.TempDir()
	if err := writeServiceBundleNet(bundle2, "/rootfs", Service{Name: "api", Image: "x"},
		[]string{"x"}, nil, nil, "", nil, "", "", nil, false); err != nil {
		t.Fatalf("writeServiceBundleNet (no resolv): %v", err)
	}
	if mountByDest(readBundleSpec(t, bundle2), "/etc/resolv.conf") != nil {
		t.Error("a service with no resolvConf must NOT mount /etc/resolv.conf")
	}
}

// TestWriteServiceBundleNet_HostsMount asserts a space service bind-mounts
// the generated hosts file read-only at /etc/hosts (the fix for images that
// resolve localhost/0.0.0.0 in a netns with no /etc/hosts), and that an empty
// hosts adds no such mount.
func TestWriteServiceBundleNet_HostsMount(t *testing.T) {
	bundle := t.TempDir()
	join := &joinNamespaces{UsernsPath: "/proc/1/ns/user", PrivateNetns: true}
	if err := writeServiceBundleNet(bundle, "/rootfs", Service{Name: "nats", Image: "x"},
		[]string{"x"}, nil, nil, "", join, "/tmp/spacenet-resolv.conf", "/tmp/spacenet-hosts", nil, false); err != nil {
		t.Fatalf("writeServiceBundleNet: %v", err)
	}
	spec := readBundleSpec(t, bundle)

	m := mountByDest(spec, "/etc/hosts")
	if m == nil {
		t.Fatal("space service must bind-mount /etc/hosts")
	}
	if m["source"] != "/tmp/spacenet-hosts" || m["type"] != "bind" {
		t.Errorf("hosts mount = %v, want bind of /tmp/spacenet-hosts", m)
	}
	foundRO := false
	for _, o := range m["options"].([]any) {
		if o == "ro" {
			foundRO = true
		}
	}
	if !foundRO {
		t.Errorf("hosts mount must be read-only, opts = %v", m["options"])
	}

	// Empty hosts → no /etc/hosts mount (CI shared-netns path).
	bundle2 := t.TempDir()
	if err := writeServiceBundleNet(bundle2, "/rootfs", Service{Name: "nats", Image: "x"},
		[]string{"x"}, nil, nil, "", nil, "", "", nil, false); err != nil {
		t.Fatalf("writeServiceBundleNet (no hosts): %v", err)
	}
	if mountByDest(readBundleSpec(t, bundle2), "/etc/hosts") != nil {
		t.Error("a service with no hosts must NOT mount /etc/hosts")
	}
}

// TestWriteServiceBundleNet_CreateRuntimeHook asserts a space service spec
// carries both network-before-exec OCI hooks: createRuntime moves the peer from
// the worker and createContainer configures it inside the container netns.
func TestWriteServiceBundleNet_CreateRuntimeHook(t *testing.T) {
	bundle := t.TempDir()
	join := &joinNamespaces{UsernsPath: "/proc/1/ns/user", PrivateNetns: true}
	hook := &createRuntimeHook{
		Path:         "/usr/bin/sklp",
		MoveEnv:      []string{"SKLP_SPACE_HOOK=move"},
		ConfigureEnv: []string{"SKLP_SPACE_HOOK=configure"},
	}
	if err := writeServiceBundleNet(bundle, "/rootfs", Service{Name: "migrate", Image: "x"},
		[]string{"x"}, nil, nil, "", join, "/tmp/r.conf", "/tmp/hosts", hook, false); err != nil {
		t.Fatalf("writeServiceBundleNet: %v", err)
	}
	spec := readBundleSpec(t, bundle)

	hooks, ok := spec["hooks"].(map[string]any)
	if !ok {
		t.Fatal("space service spec must carry hooks")
	}
	cr, ok := hooks["createRuntime"].([]any)
	if !ok || len(cr) != 1 {
		t.Fatalf("expected one createRuntime hook, got %v", hooks["createRuntime"])
	}
	h := cr[0].(map[string]any)
	if h["path"] != "/usr/bin/sklp" {
		t.Errorf("hook path = %v, want /usr/bin/sklp", h["path"])
	}
	// argv[0] must be the hook binary so the re-exec'd process identifies itself.
	args := h["args"].([]any)
	if len(args) != 1 || args[0] != "/usr/bin/sklp" {
		t.Errorf("hook args = %v, want [/usr/bin/sklp]", args)
	}
	env := h["env"].([]any)
	foundMarker := false
	for _, e := range env {
		if e == "SKLP_SPACE_HOOK=move" {
			foundMarker = true
		}
	}
	if !foundMarker {
		t.Errorf("createRuntime env must carry the move marker, got %v", env)
	}
	cc, ok := hooks["createContainer"].([]any)
	if !ok || len(cc) != 1 {
		t.Fatalf("expected one createContainer hook, got %v", hooks["createContainer"])
	}
	foundMarker = false
	for _, e := range cc[0].(map[string]any)["env"].([]any) {
		if e == "SKLP_SPACE_HOOK=configure" {
			foundMarker = true
		}
	}
	if !foundMarker {
		t.Errorf("createContainer env must carry the configure marker, got %v", cc[0])
	}

	// nil hook (CI / standalone path) → no hooks key at all.
	bundle2 := t.TempDir()
	if err := writeServiceBundleNet(bundle2, "/rootfs", Service{Name: "ci", Image: "x"},
		[]string{"x"}, nil, nil, "", nil, "", "", nil, false); err != nil {
		t.Fatalf("writeServiceBundleNet (no hook): %v", err)
	}
	if _, has := readBundleSpec(t, bundle2)["hooks"]; has {
		t.Error("a service with no hook must NOT carry a hooks key")
	}
}

func readBundleSpec(t *testing.T, bundle string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(bundle, "config.json"))
	if err != nil {
		t.Fatalf("read config.json: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode config.json: %v", err)
	}
	return m
}

// TestServiceNameOf covers the instance-id -> service-name reduction used to
// register one space DNS name shared by every replica (round-robin).
func TestServiceNameOf(t *testing.T) {
	cases := map[string]string{
		"db-0":         "db",
		"db-3":         "db",
		"web-api-0":    "web-api", // hyphenated service name, numeric replica suffix
		"db-v-0":       "db",      // rollout id, pre-fix shape (back-compat)
		"web-api-v-2":  "web-api",
		"db-v1-0":      "db", // rollout id, generation-numbered shape
		"web-api-v2-1": "web-api",
		"db":           "db", // no suffix (defensive)
	}
	for in, want := range cases {
		if got := serviceNameOf(in); got != want {
			t.Errorf("serviceNameOf(%q) = %q, want %q", in, got, want)
		}
	}
}
