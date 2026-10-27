package space

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// marshalSpec round-trips buildSpec through JSON so tests assert on the
// exact structure runc would receive, not on Go map internals.
func marshalSpec(t *testing.T, cfg Config, uid, gid int) map[string]any {
	t.Helper()
	return marshalSpecCgroups(t, "", cfg, uid, gid)
}

// marshalSpecCgroups is marshalSpec with an explicit cgroupsPath, for the
// limit-aware tests.
func marshalSpecCgroups(t *testing.T, cgroupsPath string, cfg Config, uid, gid int) map[string]any {
	t.Helper()
	b, err := json.MarshalIndent(buildSpec("/rootfs", cgroupsPath, cfg, uid, gid), "", "  ")
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal spec: %v", err)
	}
	return got
}

func TestBuildSpec_WorkspaceBindMount(t *testing.T) {
	spec := marshalSpec(t, Config{RepoPath: "/home/dev/repo"}, 1000, 1000)
	mounts, ok := spec["mounts"].([]any)
	if !ok {
		t.Fatalf("mounts missing or wrong type: %T", spec["mounts"])
	}
	var found bool
	for _, m := range mounts {
		mm := m.(map[string]any)
		if mm["destination"] != "/workspace" {
			continue
		}
		found = true
		if mm["type"] != "bind" {
			t.Errorf("workspace mount type = %v, want bind", mm["type"])
		}
		if mm["source"] != "/home/dev/repo" {
			t.Errorf("workspace mount source = %v, want /home/dev/repo", mm["source"])
		}
		opts := mm["options"].([]any)
		if len(opts) != 2 || opts[0] != "rbind" || opts[1] != "rw" {
			t.Errorf("workspace mount options = %v, want [rbind rw]", opts)
		}
	}
	if !found {
		t.Fatal("no /workspace bind mount in spec")
	}
}

// A linked worktree (`.git` is a file pointing at <main>/.git/worktrees/<name>)
// must get the main repo bind-mounted at its own absolute path, or git inside
// the container cannot resolve the pointer (breaks token wiring + go mod).
func TestBuildSpec_WorktreeMainRepoBindMount(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "main")
	wt := filepath.Join(dir, "wt")
	if err := os.MkdirAll(filepath.Join(main, ".git", "worktrees", "wt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	gitdir := filepath.Join(main, ".git", "worktrees", "wt")
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+gitdir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	spec := marshalSpec(t, Config{RepoPath: wt}, 1000, 1000)
	mounts := spec["mounts"].([]any)
	var found bool
	for _, m := range mounts {
		mm := m.(map[string]any)
		if mm["destination"] == main {
			found = true
			if mm["source"] != main {
				t.Errorf("worktree main mount source = %v, want %v", mm["source"], main)
			}
			opts := mm["options"].([]any)
			if len(opts) != 2 || opts[1] != "ro" {
				t.Errorf("worktree main mount options = %v, want [rbind ro]", opts)
			}
		}
	}
	if !found {
		t.Fatalf("no bind mount for main repo %s in worktree spec", main)
	}
}

// The CI-via-space path (ci_space.go) runs `sklp run` inside a SERVICE
// container, not the buildSpec runner. It must apply the SAME worktree
// fix: when /workspace is a linked worktree, the main repo is bind-mounted
// (ro) at its absolute host path via a DevMount, or git steps like
// branch-context die with exit 128 "not a git repository". This guards that
// the service spec materialises that DevMount correctly.
func TestServiceSpec_WorktreeMainRepoDevMount(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "main")
	wt := filepath.Join(dir, "wt")
	if err := os.MkdirAll(filepath.Join(main, ".git", "worktrees", "wt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	gitdir := filepath.Join(main, ".git", "worktrees", "wt")
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+gitdir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := mainRepoForWorktree(wt)
	if got != main {
		t.Fatalf("mainRepoForWorktree(%s) = %q, want %q", wt, got, main)
	}

	// Same DevMounts ci_space.go now builds: /workspace + the ro main repo.
	svc := Service{Name: "runner", DevMounts: []DevMount{
		{Source: wt, Destination: "/workspace"},
		{Source: got, Destination: got, ReadOnly: true},
	}}
	spec := buildServiceSpec("/rootfs", svc, nil, nil, nil, "/workspace", 1000, 1000, nil)
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, raw := range m["mounts"].([]any) {
		mm := raw.(map[string]any)
		if mm["destination"] != main {
			continue
		}
		found = true
		if mm["source"] != main {
			t.Errorf("main repo mount source = %v, want %v", mm["source"], main)
		}
		opts := mm["options"].([]any)
		if len(opts) != 2 || opts[1] != "ro" {
			t.Errorf("main repo mount options = %v, want [rbind ro]", opts)
		}
	}
	if !found {
		t.Fatalf("service spec missing ro bind for main repo %s", main)
	}
}

// A plain checkout (`.git` is a directory) must NOT add a second bind mount.
func TestBuildSpec_PlainCheckoutNoExtraMount(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	spec := marshalSpec(t, Config{RepoPath: dir}, 1000, 1000)
	mounts := spec["mounts"].([]any)
	if len(mounts) != 4 { // proc, cgroup, /workspace, /tmp
		t.Fatalf("plain checkout produced %d mounts, want 4: %v", len(mounts), mounts)
	}
}

func TestBuildSpec_HomeIsTmp(t *testing.T) {
	spec := marshalSpec(t, Config{RepoPath: "/repo"}, 1000, 1000)
	env := spec["process"].(map[string]any)["env"].([]any)
	var hasHome bool
	for _, e := range env {
		if e == "HOME=/tmp" {
			hasHome = true
		}
	}
	if !hasHome {
		t.Errorf("HOME=/tmp not in process env: %v", env)
	}
}

func TestBuildSpec_CallerEnvLayeredAfterDefaults(t *testing.T) {
	spec := marshalSpec(t, Config{RepoPath: "/repo", Env: []string{"FOO=bar"}}, 1000, 1000)
	env := spec["process"].(map[string]any)["env"].([]any)
	// Defaults come first, caller env after, so duplicates from the
	// caller win at the OCI layer.
	if env[0] != "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" {
		t.Errorf("env[0] = %v, want PATH default first", env[0])
	}
	if env[len(env)-1] != "FOO=bar" {
		t.Errorf("caller env not appended last: %v", env)
	}
}

func TestBuildSpec_UserNamespaceRemap(t *testing.T) {
	spec := marshalSpec(t, Config{RepoPath: "/repo"}, 1234, 5678)
	linux := spec["linux"].(map[string]any)

	uidMaps := linux["uidMappings"].([]any)
	if len(uidMaps) != 2 {
		t.Fatalf("uidMappings len = %d, want 2", len(uidMaps))
	}
	root := uidMaps[0].(map[string]any)
	if root["containerID"].(float64) != 0 || root["hostID"].(float64) != 1234 || root["size"].(float64) != 1 {
		t.Errorf("uid root map = %v, want {0 -> 1234 size 1}", root)
	}
	rest := uidMaps[1].(map[string]any)
	if rest["containerID"].(float64) != 1 || rest["hostID"].(float64) != 100000 || rest["size"].(float64) != 65536 {
		t.Errorf("uid subrange map = %v, want {1 -> 100000 size 65536}", rest)
	}

	gidMaps := linux["gidMappings"].([]any)
	gRoot := gidMaps[0].(map[string]any)
	if gRoot["hostID"].(float64) != 5678 {
		t.Errorf("gid root hostID = %v, want 5678", gRoot["hostID"])
	}
}

func TestBuildSpec_UserNamespacePresent(t *testing.T) {
	spec := marshalSpec(t, Config{RepoPath: "/repo"}, 1000, 1000)
	ns := spec["linux"].(map[string]any)["namespaces"].([]any)
	var hasUser bool
	for _, n := range ns {
		if n.(map[string]any)["type"] == "user" {
			hasUser = true
		}
	}
	if !hasUser {
		t.Error("user namespace missing from spec")
	}
}

func TestBuildSpec_NoSysNoDevPts(t *testing.T) {
	spec := marshalSpec(t, Config{RepoPath: "/repo"}, 1000, 1000)
	mounts := spec["mounts"].([]any)
	for _, m := range mounts {
		dst := m.(map[string]any)["destination"]
		if dst == "/sys" {
			t.Error("/sys mount present; rootless without netns alloc cannot mount sysfs")
		}
		// The runner is non-interactive (no `exec -t`), so it has no devpts —
		// unlike a space SERVICE (see TestBuildServiceSpec_DevPtsForInteractiveExec).
		if dst == "/dev/pts" {
			t.Error("/dev/pts mount present on the runner spec; only service specs need it")
		}
	}
}

// TestBuildServiceSpec_DevPtsForInteractiveExec is the regression for the silent
// "TTY exec shows nothing" bug: a space service with no /dev/pts has no
// /dev/pts/ptmx, so `runc exec -t` (the interactive `sklp deploy exec` path)
// fails to allocate a console and the session is mute. The service spec must
// carry a private (newinstance) devpts mount with a world-openable ptmx.
func TestBuildServiceSpec_DevPtsForInteractiveExec(t *testing.T) {
	b, err := json.MarshalIndent(
		buildServiceSpec("/rootfs", Service{Name: "web"}, nil, []string{"nginx"}, nil, "", 1000, 1000, nil),
		"", "  ")
	if err != nil {
		t.Fatalf("marshal service spec: %v", err)
	}
	var spec map[string]any
	if err := json.Unmarshal(b, &spec); err != nil {
		t.Fatalf("unmarshal service spec: %v", err)
	}
	mounts := spec["mounts"].([]any)
	var found map[string]any
	for _, m := range mounts {
		mm := m.(map[string]any)
		if mm["destination"] == "/dev/pts" {
			found = mm
			break
		}
	}
	if found == nil {
		t.Fatal("service spec has no /dev/pts mount: runc exec -t cannot allocate a console")
	}
	if found["type"] != "devpts" || found["source"] != "devpts" {
		t.Errorf("/dev/pts mount = type %v source %v, want devpts/devpts", found["type"], found["source"])
	}
	opts := make(map[string]bool)
	for _, o := range found["options"].([]any) {
		opts[o.(string)] = true
	}
	// newinstance gives the container its own pts namespace; ptmxmode=0666 lets the
	// unprivileged container uid open the multiplexor (no host tty gid is mapped in).
	if !opts["newinstance"] || !opts["ptmxmode=0666"] {
		t.Errorf("/dev/pts options = %v, want newinstance + ptmxmode=0666", found["options"])
	}
}

// TestBuildServiceSpec_NoDevMounts_DeploySpecUnchanged is the security guard
// for the deploy frontier: a service WITHOUT DevMounts (the deploy/CI path)
// must carry ONLY the fixed kernel/runtime mounts and NO host bind-mount. The
// fixed set is /proc, /sys (ro), /tmp, /run, /var/run, /dev/shm (the tmpfs that
// back a read-only rootfs) and /dev/pts. The read-only rootfs is asserted too: a
// service can never mutate its own image.
func TestBuildServiceSpec_NoDevMounts_DeploySpecUnchanged(t *testing.T) {
	spec := buildServiceSpec("/rootfs", Service{Name: "db"}, nil, []string{"postgres"}, nil, "", 1000, 1000, nil)

	// Immutable rootfs is the isolation guarantee.
	if ro := spec["root"].(map[string]any)["readonly"]; ro != true {
		t.Errorf("service rootfs must be read-only, got readonly=%v", ro)
	}

	mounts := spec["mounts"].([]map[string]any)
	want := map[string]bool{"/proc": true, "/sys": true, "/tmp": true, "/run": true, "/var/run": true, "/dev/shm": true, "/dev/pts": true}
	if len(mounts) != len(want) {
		t.Fatalf("deploy service spec has %d mounts, want exactly %d (%v); host bind leaked: %v",
			len(mounts), len(want), keysOf(want), mounts)
	}
	for _, m := range mounts {
		dst := m["destination"].(string)
		if !want[dst] {
			t.Errorf("unexpected mount %q in deploy spec", dst)
		}
		if m["type"] == "bind" {
			t.Errorf("deploy spec must not carry a host bind mount, found bind at %q", dst)
		}
	}
}

// keysOf returns the keys of a string-set, for readable test failures.
func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestBuildServiceSpec_DevMount_AppendsRWBind proves the dev hot-reload path:
// a DevMount produces an rbind+rw mount at the destination. The host uid
// mapping (uid 0 -> host uid) is unchanged, so the container writes to the
// repo as the operator, never root.
func TestBuildServiceSpec_DevMount_AppendsRWBind(t *testing.T) {
	svc := Service{Name: "core", DevMounts: []DevMount{{Source: "/home/dev/repo", Destination: "/workspace"}}}
	spec := buildServiceSpec("/rootfs", svc, []string{"go"}, []string{"run", "."}, nil, "/workspace", 4242, 4242, nil)

	var found map[string]any
	for _, m := range spec["mounts"].([]map[string]any) {
		if m["destination"] == "/workspace" {
			found = m
		}
	}
	if found == nil {
		t.Fatal("dev bind mount /workspace missing from spec")
	}
	if found["type"] != "bind" || found["source"] != "/home/dev/repo" {
		t.Errorf("dev mount = type %v source %v, want bind /home/dev/repo", found["type"], found["source"])
	}
	opts := found["options"].([]string)
	if len(opts) != 2 || opts[0] != "rbind" || opts[1] != "rw" {
		t.Errorf("dev mount options = %v, want [rbind rw]", opts)
	}
	// uid 0 maps to the host operator uid: repo writes stay operator-owned.
	uidMaps := spec["linux"].(map[string]any)["uidMappings"].([]map[string]any)
	if uidMaps[0]["containerID"] != 0 || uidMaps[0]["hostID"] != 4242 {
		t.Errorf("uid root map = %v, want {0 -> 4242}", uidMaps[0])
	}
}

// TestBuildServiceSpec_DevMount_ReadOnly checks the ro variant binds ro.
func TestBuildServiceSpec_DevMount_ReadOnly(t *testing.T) {
	svc := Service{Name: "core", DevMounts: []DevMount{{Source: "/etc/app.conf", Destination: "/app.conf", ReadOnly: true}}}
	spec := buildServiceSpec("/rootfs", svc, []string{"sh"}, nil, nil, "", 1000, 1000, nil)
	for _, m := range spec["mounts"].([]map[string]any) {
		if m["destination"] == "/app.conf" {
			opts := m["options"].([]string)
			if opts[1] != "ro" {
				t.Errorf("read-only dev mount options = %v, want ro", opts)
			}
			return
		}
	}
	t.Fatal("read-only dev mount missing")
}

// nsTypes returns the set of namespace "type" values declared in a service spec.
func nsTypes(t *testing.T, spec map[string]any) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, n := range spec["linux"].(map[string]any)["namespaces"].([]map[string]any) {
		out[n["type"].(string)] = true
	}
	return out
}

// TestBuildServiceSpec_PrivateNetnsByDefault locks the default: a service WITHOUT
// HostNetwork keeps its own private network namespace (the {"type":"network"}
// entry). This is the standalone/space path and must not regress.
func TestBuildServiceSpec_PrivateNetnsByDefault(t *testing.T) {
	spec := buildServiceSpec("/rootfs", Service{Name: "web"}, nil, []string{"nginx"}, nil, "", 1000, 1000, nil)
	ns := nsTypes(t, spec)
	if !ns["network"] {
		t.Error("default service spec is missing the private network namespace")
	}
	if !ns["user"] {
		t.Error("default service spec is missing the user namespace")
	}
}

func TestBuildServiceSpec_PrivateNetnsOpensLowPorts(t *testing.T) {
	spec := buildServiceSpec("/rootfs", Service{Name: "web"}, nil, []string{"nginx"}, nil, "", 1000, 1000, nil)
	sysctl, _ := spec["linux"].(map[string]any)["sysctl"].(map[string]string)
	if got := sysctl["net.ipv4.ip_unprivileged_port_start"]; got != "0" {
		t.Errorf("private-netns service must bind ports below 1024 without a capability, sysctl = %q", got)
	}
	for _, c := range spec["process"].(map[string]any)["capabilities"].(map[string]any)["bounding"].([]string) {
		if c == "CAP_NET_BIND_SERVICE" {
			t.Error("private-netns service must not get CAP_NET_BIND_SERVICE by default")
		}
	}
}

func TestBuildServiceSpec_HostNetworkLeavesNodeThresholdAlone(t *testing.T) {
	spec := buildServiceSpec("/rootfs", Service{Name: "gateway", HostNetwork: true}, nil, []string{"traefik"}, nil, "", 1000, 1000, nil)
	if sysctl, ok := spec["linux"].(map[string]any)["sysctl"]; ok {
		t.Errorf("host-network service must not set sysctls on the node's netns, got %v", sysctl)
	}
}

// TestBuildServiceSpec_HostNetworkOmitsNetns is the G3c gate: a service WITH
// HostNetwork omits the network namespace (so the container shares the host
// netns and binds host ports directly) while KEEPING the user namespace +
// uid/gid mappings (filesystem/uid isolation is independent of the netns).
func TestBuildServiceSpec_HostNetworkOmitsNetns(t *testing.T) {
	svc := Service{Name: "gateway", HostNetwork: true}
	spec := buildServiceSpec("/rootfs", svc, nil, []string{"traefik"}, nil, "", 1000, 1000, nil)
	ns := nsTypes(t, spec)
	if ns["network"] {
		t.Error("HostNetwork spec must OMIT the network namespace so it shares the host netns")
	}
	// User namespace + mappings are retained for isolation; only the netns goes.
	if !ns["user"] {
		t.Error("HostNetwork spec must keep the user namespace")
	}
	uidMaps := spec["linux"].(map[string]any)["uidMappings"].([]map[string]any)
	if len(uidMaps) != 2 || uidMaps[0]["hostID"] != 1000 {
		t.Errorf("HostNetwork spec must keep uid mappings, got %v", uidMaps)
	}
	// CAP_NET_BIND_SERVICE must be present so traefik can bind :443/:80.
	caps := spec["process"].(map[string]any)["capabilities"].(map[string]any)["bounding"].([]string)
	var hasBind bool
	for _, c := range caps {
		if c == "CAP_NET_BIND_SERVICE" {
			hasBind = true
		}
	}
	if !hasBind {
		t.Error("HostNetwork spec must grant CAP_NET_BIND_SERVICE to bind privileged host ports")
	}
}

// effectivePATH returns the last PATH= entry in env (OCI last-wins), or "" if
// none. It mirrors how runc resolves a duplicated env key inside the container.
func effectivePATH(env []string) string {
	out := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			out = kv[len("PATH="):]
		}
	}
	return out
}

// TestBuildServiceSpec_ImagePATHWins guards the initdb regression: when the
// image config carries its own PATH, the effective container PATH must be the
// image's — no generic default may shadow it, or postgres can't find initdb.
func TestBuildServiceSpec_ImagePATHWins(t *testing.T) {
	imgEnv := []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/lib/postgresql/17/bin", "POSTGRES=1"}
	spec := buildServiceSpec("/rootfs", Service{Name: "db"}, nil, []string{"postgres"}, imgEnv, "", 1000, 1000, nil)
	env := spec["process"].(map[string]any)["env"].([]string)
	if got := effectivePATH(env); got != "/usr/local/sbin:/usr/local/bin:/usr/lib/postgresql/17/bin" {
		t.Errorf("effective PATH = %q, want the image PATH (initdb dir)", got)
	}
	// No host shell PATH may leak in.
	for _, kv := range env {
		if strings.Contains(kv, "/home/") || strings.Contains(kv, ".cargo") {
			t.Errorf("host shell env leaked into service spec: %q", kv)
		}
	}
}

// TestBuildServiceSpec_FallbackPATHWhenImageHasNone keeps a usable PATH for
// images whose config declares none (rare, e.g. scratch-based).
func TestBuildServiceSpec_FallbackPATHWhenImageHasNone(t *testing.T) {
	spec := buildServiceSpec("/rootfs", Service{Name: "x"}, nil, []string{"app"}, []string{"FOO=bar"}, "", 1000, 1000, nil)
	env := spec["process"].(map[string]any)["env"].([]string)
	if got := effectivePATH(env); got != "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" {
		t.Errorf("fallback PATH = %q, want the default", got)
	}
}

func TestBuildSpec_NoSysAdminCapability(t *testing.T) {
	spec := marshalSpec(t, Config{RepoPath: "/repo"}, 1000, 1000)
	caps := spec["process"].(map[string]any)["capabilities"].(map[string]any)
	for _, set := range []string{"bounding", "effective", "permitted"} {
		for _, c := range caps[set].([]any) {
			if c == "CAP_SYS_ADMIN" {
				t.Errorf("CAP_SYS_ADMIN present in %s set; must never be granted", set)
			}
		}
	}
}

func TestBuildSpec_TaskNameDefaultsToCI(t *testing.T) {
	spec := marshalSpec(t, Config{RepoPath: "/repo"}, 1000, 1000)
	args := spec["process"].(map[string]any)["args"].([]any)
	// sklp run ci -repo /workspace -output json
	if args[2] != "ci" {
		t.Errorf("default task = %v, want ci (args=%v)", args[2], args)
	}
}

func TestBuildSpec_ExtraArgsAppended(t *testing.T) {
	spec := marshalSpec(t, Config{RepoPath: "/repo", TaskName: "build", ExtraArgs: []string{"-no-pull"}}, 1000, 1000)
	args := spec["process"].(map[string]any)["args"].([]any)
	if args[2] != "build" {
		t.Errorf("task = %v, want build", args[2])
	}
	if args[len(args)-1] != "-no-pull" {
		t.Errorf("ExtraArgs not appended: %v", args)
	}
}

func TestBuildSpec_CwdAndRootfs(t *testing.T) {
	spec := marshalSpec(t, Config{RepoPath: "/repo"}, 1000, 1000)
	if cwd := spec["process"].(map[string]any)["cwd"]; cwd != "/workspace" {
		t.Errorf("cwd = %v, want /workspace", cwd)
	}
	if path := spec["root"].(map[string]any)["path"]; path != "/rootfs" {
		t.Errorf("root path = %v, want /rootfs", path)
	}
}

func TestBuildSpec_NoNewPrivileges(t *testing.T) {
	spec := marshalSpec(t, Config{RepoPath: "/repo"}, 1000, 1000)
	if v := spec["process"].(map[string]any)["noNewPrivileges"]; v != true {
		t.Errorf("noNewPrivileges = %v, want true", v)
	}
}

func TestDefaultCaps_ExcludesSysAdmin(t *testing.T) {
	for _, c := range defaultCaps() {
		if c == "CAP_SYS_ADMIN" {
			t.Fatal("defaultCaps includes CAP_SYS_ADMIN")
		}
	}
}

func TestBuildSpec_NoLimits_NoResourcesOrCgroupsPath(t *testing.T) {
	spec := marshalSpec(t, Config{RepoPath: "/repo"}, 1000, 1000)
	linux := spec["linux"].(map[string]any)
	if _, ok := linux["resources"]; ok {
		t.Error("linux.resources present without any limit; must stay absent")
	}
	if _, ok := linux["cgroupsPath"]; ok {
		t.Error("linux.cgroupsPath present without any limit; must stay absent")
	}
}

func TestBuildSpec_MemoryLimit(t *testing.T) {
	spec := marshalSpec(t, Config{RepoPath: "/repo", MemoryLimit: 1 << 30}, 1000, 1000)
	res := spec["linux"].(map[string]any)["resources"].(map[string]any)
	mem := res["memory"].(map[string]any)
	if mem["limit"].(float64) != float64(1<<30) {
		t.Errorf("memory.limit = %v, want %d", mem["limit"], 1<<30)
	}
	if _, ok := res["cpu"]; ok {
		t.Error("cpu subsection present for memory-only limit")
	}
}

func TestBuildSpec_CPULimit(t *testing.T) {
	spec := marshalSpec(t, Config{RepoPath: "/repo", CPULimit: 2.0}, 1000, 1000)
	res := spec["linux"].(map[string]any)["resources"].(map[string]any)
	cpu := res["cpu"].(map[string]any)
	if cpu["quota"].(float64) != 200000 || cpu["period"].(float64) != 100000 {
		t.Errorf("cpu quota/period = %v/%v, want 200000/100000", cpu["quota"], cpu["period"])
	}
	if _, ok := res["memory"]; ok {
		t.Error("memory subsection present for cpu-only limit")
	}
}

func TestBuildSpec_CPULimitFractional(t *testing.T) {
	spec := marshalSpec(t, Config{RepoPath: "/repo", CPULimit: 0.5}, 1000, 1000)
	cpu := spec["linux"].(map[string]any)["resources"].(map[string]any)["cpu"].(map[string]any)
	if cpu["quota"].(float64) != 50000 || cpu["period"].(float64) != 100000 {
		t.Errorf("cpu quota/period = %v/%v, want 50000/100000", cpu["quota"], cpu["period"])
	}
}

func TestBuildSpec_BothLimits(t *testing.T) {
	spec := marshalSpec(t, Config{RepoPath: "/repo", CPULimit: 1.0, MemoryLimit: 256 << 20}, 1000, 1000)
	res := spec["linux"].(map[string]any)["resources"].(map[string]any)
	if _, ok := res["cpu"]; !ok {
		t.Error("cpu subsection missing when both limits set")
	}
	if _, ok := res["memory"]; !ok {
		t.Error("memory subsection missing when both limits set")
	}
}

func TestBuildSpec_CgroupsPathSetWhenProvided(t *testing.T) {
	spec := marshalSpecCgroups(t, "user.slice:sklp:sklp-ci",
		Config{RepoPath: "/repo", MemoryLimit: 1 << 20}, 1000, 1000)
	if path := spec["linux"].(map[string]any)["cgroupsPath"]; path != "user.slice:sklp:sklp-ci" {
		t.Errorf("cgroupsPath = %v, want user.slice:sklp:sklp-ci", path)
	}
}

// TestWriteBundle_ConfigPerms guards the secrets-on-disk regression: config.json
// embeds every secret in process.env, so it must be written 0600 inside a 0700
// bundle dir — never world/group-readable, regardless of cache-dir inheritance.
func TestWriteBundle_ConfigPerms(t *testing.T) {
	bundle := filepath.Join(t.TempDir(), "bundles", "run-ci")
	if err := mkdirBundle(bundle); err != nil {
		t.Fatalf("mkdirBundle: %v", err)
	}
	if err := writeRunnerBundle(bundle, "/rootfs", "", Config{RepoPath: "/repo"}, nil); err != nil {
		t.Fatalf("writeRunnerBundle: %v", err)
	}

	cfg, err := os.Stat(filepath.Join(bundle, "config.json"))
	if err != nil {
		t.Fatalf("stat config.json: %v", err)
	}
	if perm := cfg.Mode().Perm(); perm != 0o600 {
		t.Errorf("config.json mode = %o, want 600", perm)
	}

	dir, err := os.Stat(bundle)
	if err != nil {
		t.Fatalf("stat bundle: %v", err)
	}
	if perm := dir.Mode().Perm(); perm != 0o700 {
		t.Errorf("bundle dir mode = %o, want 700", perm)
	}
}

// hasHardening asserts a spec's linux object carries the seccomp allowlist plus
// masked/readonly paths — the byte-for-byte isolation floor every spec producer
// must share. Returns the seccomp default action for callers that want it.
func hasHardening(t *testing.T, spec map[string]any) string {
	t.Helper()
	linux := spec["linux"].(map[string]any)
	if _, ok := linux["maskedPaths"]; !ok {
		t.Error("spec linux is missing maskedPaths")
	}
	if _, ok := linux["readonlyPaths"]; !ok {
		t.Error("spec linux is missing readonlyPaths")
	}
	sc, ok := linux["seccomp"].(map[string]any)
	if !ok {
		t.Fatal("spec linux is missing the seccomp profile")
	}
	return sc["defaultAction"].(string)
}

// TestRunnerSpec_Hardened gates the runner/CI path: a seccomp allowlist + masked
// /proc paths, denied-by-errno default.
func TestRunnerSpec_Hardened(t *testing.T) {
	spec := buildSpec("/rootfs", "", Config{RepoPath: "/repo"}, 1000, 1000)
	if act := hasHardening(t, spec); act != "SCMP_ACT_ERRNO" {
		t.Errorf("runner seccomp defaultAction = %q, want SCMP_ACT_ERRNO", act)
	}
}

// TestServiceSpec_Hardened gates the service path, including the HostNetwork
// variant — sharing the host netns must NOT drop seccomp/masked-paths.
func TestServiceSpec_Hardened(t *testing.T) {
	for _, hostNet := range []bool{false, true} {
		svc := Service{Name: "svc", HostNetwork: hostNet}
		spec := buildServiceSpec("/rootfs", svc, nil, []string{"nginx"}, nil, "", 1000, 1000, nil)
		if act := hasHardening(t, spec); act != "SCMP_ACT_ERRNO" {
			t.Errorf("service(hostNet=%v) seccomp defaultAction = %q, want SCMP_ACT_ERRNO", hostNet, act)
		}
	}
}

// TestApplyOverlayRoot is the QA guard for the writable-overlay-rootfs path
// (PR #695). It asserts the four mutations applyOverlayRoot makes to a
// space-join spec: (1) root becomes writable, (2) the masking /tmp /run /var/run
// tmpfs are dropped (they'd hide image content over the overlay), (3) the user
// namespace + uid/gid mappings are dropped (runc inherits them via nsenter),
// and (4) /proc, /dev/pts and /sys survive.
func TestApplyOverlayRoot(t *testing.T) {
	join := &joinNamespaces{UsernsPath: "/proc/7/ns/user", PrivateNetns: true}
	spec := buildServiceSpec("/rootfs", Service{Name: "db"}, nil, []string{"mariadb"}, nil, "", 1000, 1000, join)
	applyOverlayRoot(spec)

	// (1) root writable.
	root := spec["root"].(map[string]any)
	if root["readonly"] != false {
		t.Errorf("root.readonly = %v, want false (writable overlay)", root["readonly"])
	}

	// (2) masking tmpfs dropped; (4) kernel mounts kept.
	mounts := spec["mounts"].([]map[string]any)
	gotDest := map[string]bool{}
	for _, m := range mounts {
		gotDest[m["destination"].(string)] = true
	}
	for _, d := range []string{"/tmp", "/run", "/var/run"} {
		if gotDest[d] {
			t.Errorf("mount %s present, want it dropped (masks image content over overlay)", d)
		}
	}
	for _, d := range []string{"/proc", "/sys", "/dev/pts"} {
		if !gotDest[d] {
			t.Errorf("mount %s missing, want it kept", d)
		}
	}

	// (3) user ns + mappings dropped, mount/network kept.
	linux := spec["linux"].(map[string]any)
	if _, ok := linux["uidMappings"]; ok {
		t.Error("uidMappings present, want dropped (inherited via nsenter)")
	}
	if _, ok := linux["gidMappings"]; ok {
		t.Error("gidMappings present, want dropped (inherited via nsenter)")
	}
	nsTypes := map[string]bool{}
	for _, ns := range linux["namespaces"].([]map[string]any) {
		nsTypes[ns["type"].(string)] = true
	}
	if nsTypes["user"] {
		t.Error("user namespace present, want dropped (runc enters holder's userns)")
	}
	if !nsTypes["mount"] || !nsTypes["network"] {
		t.Errorf("namespaces = %v, want mount+network kept", nsTypes)
	}
}

func TestBuildServiceSpec_NestedRuntimeCanRunRunc(t *testing.T) {
	spec := buildServiceSpec("/rootfs", Service{Name: "runner", NestedRuntime: true}, nil, []string{"sklp"}, nil, "", 1000, 1000, nil)
	linux := spec["linux"].(map[string]any)

	for _, p := range append(linux["maskedPaths"].([]string), linux["readonlyPaths"].([]string)...) {
		if strings.HasPrefix(p, "/proc/") {
			t.Errorf("%s is mounted over /proc; a nested proc mount is then refused with EPERM", p)
		}
	}
	caps := spec["process"].(map[string]any)["capabilities"].(map[string]any)["bounding"].([]string)
	if !slices.Contains(caps, "CAP_SYS_ADMIN") {
		t.Error("CAP_SYS_ADMIN missing: runc cannot setns into the step's mount namespace")
	}
	var allowed []string
	for _, rule := range linux["seccomp"].(map[string]any)["syscalls"].([]map[string]any) {
		allowed = append(allowed, rule["names"].([]string)...)
	}
	if !slices.Contains(allowed, "pivot_root") {
		t.Error("pivot_root denied by seccomp: runc cannot jail the step")
	}
	var hasCgroupNS, hasCgroupMount, hasTun bool
	for _, ns := range linux["namespaces"].([]map[string]any) {
		hasCgroupNS = hasCgroupNS || ns["type"] == "cgroup"
	}
	for _, m := range spec["mounts"].([]map[string]any) {
		hasCgroupMount = hasCgroupMount || m["destination"] == "/sys/fs/cgroup"
	}
	for _, d := range linux["devices"].([]map[string]any) {
		hasTun = hasTun || d["path"] == "/dev/net/tun"
	}
	if !hasCgroupNS || !hasCgroupMount {
		t.Error("runc refuses to start without a cgroup mount in mountinfo")
	}
	if !hasTun {
		t.Error("/dev/net/tun missing: the step's slirp4netns dies and the RUN has no egress")
	}
}

func TestBuildServiceSpec_DefaultServiceStaysConfined(t *testing.T) {
	spec := buildServiceSpec("/rootfs", Service{Name: "db"}, nil, []string{"postgres"}, nil, "", 1000, 1000, nil)
	linux := spec["linux"].(map[string]any)
	caps := spec["process"].(map[string]any)["capabilities"].(map[string]any)["bounding"].([]string)
	if slices.Contains(caps, "CAP_SYS_ADMIN") {
		t.Error("an application service must not get CAP_SYS_ADMIN")
	}
	if _, ok := linux["devices"]; ok {
		t.Error("an application service must not get extra devices")
	}
	if len(linux["readonlyPaths"].([]string)) == 0 {
		t.Error("an application service keeps its read-only /proc paths")
	}
	for _, m := range spec["mounts"].([]map[string]any) {
		if m["destination"] == "/sys/fs/cgroup" {
			t.Error("an application service must not see a cgroup mount; timescaledb-tune reads it and panics")
		}
	}
}

// An image that declares USER must get it. nginx-unprivileged reacts to
// finding itself root by falling back to the config it was packaged with —
// which binds :80, a port a rootless runtime grants to nobody — so dropping
// the image's uid turns a working image into a crash loop.
func TestServiceSpecHonoursTheImageUser(t *testing.T) {
	svc := Service{Name: "ui", ImageUID: 101, ImageGID: 101}
	spec := buildServiceSpec("/rootfs", svc, nil, []string{"nginx"}, nil, "", 1000, 1000, nil)

	user := spec["process"].(map[string]any)["user"].(map[string]any)
	if user["uid"] != 101 || user["gid"] != 101 {
		t.Fatalf("process.user = %v, want uid/gid 101", user)
	}
}

// An image with no USER runs as root, which is the OCI default.
func TestServiceSpecDefaultsToRootWithoutAnImageUser(t *testing.T) {
	spec := buildServiceSpec("/rootfs", Service{Name: "core"}, nil, []string{"core"}, nil, "", 1000, 1000, nil)

	user := spec["process"].(map[string]any)["user"].(map[string]any)
	if user["uid"] != 0 || user["gid"] != 0 {
		t.Fatalf("process.user = %v, want uid/gid 0", user)
	}
}

func TestImageUser_ResolvesNamesFromTheImage(t *testing.T) {
	rootfs := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootfs, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	passwd := "root:x:0:0:root:/root:/bin/sh\npostgres:x:70:70::/var/lib/postgresql:/bin/sh\n"
	group := "root:x:0:\npostgres:x:70:\nbackup:x:34:\n"
	if err := os.WriteFile(filepath.Join(rootfs, "etc", "passwd"), []byte(passwd), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootfs, "etc", "group"), []byte(group), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		user     string
		uid, gid int
		known    bool
	}{
		{"", 0, 0, true},
		{"101", 101, 0, true},
		{"101:101", 101, 101, true},
		{"postgres", 70, 70, true},
		{"postgres:backup", 70, 34, true},
		{"postgres:34", 70, 34, true},
		{"70:postgres", 70, 70, true},
		{"nginx", 0, 0, false},
		{"postgres:nogroup", 70, 0, false},
	} {
		uid, gid, known := imageUser(rootfs, c.user)
		if uid != c.uid || gid != c.gid || known != c.known {
			t.Errorf("imageUser(%q) = (%d, %d, %v), want (%d, %d, %v)", c.user, uid, gid, known, c.uid, c.gid, c.known)
		}
	}
	if uid, gid, known := imageUser(t.TempDir(), "postgres"); uid != 0 || gid != 0 || known {
		t.Errorf("an image without etc/passwd: got (%d, %d, %v), want root and unknown", uid, gid, known)
	}
}
