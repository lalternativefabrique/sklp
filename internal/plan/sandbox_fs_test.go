package plan

import (
	"reflect"
	"testing"
)

// findAccess returns the fsAccess of a given kind ("rw"/"ro"/"rwfile")
// from a policy, or an empty one when absent.
func findAccess(policy []fsAccess, kind string) fsAccess {
	for _, a := range policy {
		if a.Kind == kind {
			return a
		}
	}
	return fsAccess{}
}

func containsPath(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}

// TestComputeFSPolicy_Confined pins the default (confined) grants: the
// workdir, /tmp and the secret dir are read-write; tool dirs, cache and
// system roots are read-only; $HOME is never granted (deny-by-default).
func TestComputeFSPolicy_Confined(t *testing.T) {
	spec := sandboxSpec{
		Repo:      "/home/u/proj",
		ToolDirs:  []string{"/opt/sklp/tools/bin"},
		CacheDirs: []string{"/home/u/.cache/sklp/tools"},
		SecretDir: "/run/secrets",
	}
	policy := computeFSPolicy(spec, "/home/u/proj/services/api")
	if policy == nil {
		t.Fatal("confined policy must not be nil")
	}

	rw := findAccess(policy, "rw")
	if !containsPath(rw.Paths, "/home/u/proj/services/api") {
		t.Errorf("workdir not rw: %v", rw.Paths)
	}
	if !containsPath(rw.Paths, "/tmp") {
		t.Errorf("/tmp not rw: %v", rw.Paths)
	}
	if !containsPath(rw.Paths, "/run/secrets") {
		t.Errorf("secret dir not rw: %v", rw.Paths)
	}

	ro := findAccess(policy, "ro")
	if !containsPath(ro.Paths, "/opt/sklp/tools/bin") {
		t.Errorf("tool dir not ro: %v", ro.Paths)
	}
	if !containsPath(ro.Paths, "/home/u/.cache/sklp/tools") {
		t.Errorf("cache dir not ro: %v", ro.Paths)
	}
	for _, root := range []string{"/usr", "/etc", "/proc", "/dev", "/bin"} {
		if !containsPath(ro.Paths, root) {
			t.Errorf("system root %q not ro: %v", root, ro.Paths)
		}
	}

	// The home dir itself (outside the granted cache subpath) must never
	// appear in any grant — that is the whole point of confinement.
	for _, a := range policy {
		if containsPath(a.Paths, "/home/u") {
			t.Errorf("$HOME granted as %s — must be denied: %v", a.Kind, a.Paths)
		}
	}
}

// TestComputeFSPolicy_RepoWhenNoCwd falls back to the repo root for the
// rw grant when no per-step cwd subtree is resolved.
func TestComputeFSPolicy_RepoWhenNoCwd(t *testing.T) {
	spec := sandboxSpec{Repo: "/home/u/proj"}
	policy := computeFSPolicy(spec, "")
	rw := findAccess(policy, "rw")
	if !containsPath(rw.Paths, "/home/u/proj") {
		t.Errorf("repo root not rw when workdir empty: %v", rw.Paths)
	}
}

// TestComputeFSPolicy_TMPDIR grants a non-default TMPDIR read-write so a
// step relying on it still works under confinement.
func TestComputeFSPolicy_TMPDIR(t *testing.T) {
	spec := sandboxSpec{
		Repo:    "/home/u/proj",
		StepEnv: map[string]string{"TMPDIR": "/var/custom-tmp"},
	}
	rw := findAccess(computeFSPolicy(spec, ""), "rw")
	if !containsPath(rw.Paths, "/var/custom-tmp") {
		t.Errorf("custom TMPDIR not rw: %v", rw.Paths)
	}
	if !containsPath(rw.Paths, "/tmp") {
		t.Errorf("/tmp still expected even with TMPDIR: %v", rw.Paths)
	}
}

// TestComputeFSPolicy_DockerConfigRW grants ~/.docker read-write for a
// docker buildx step (buildx flocks ~/.docker/buildx/.lock and mutates its
// store), while still denying bare $HOME.
func TestComputeFSPolicy_DockerConfigRW(t *testing.T) {
	spec := sandboxSpec{
		Repo:            "/home/u/proj",
		Cmdline:         "docker buildx build --push -t img:tag .",
		DockerConfigDir: "/home/u/.docker",
	}
	rw := findAccess(computeFSPolicy(spec, "/home/u/proj"), "rw")
	if !containsPath(rw.Paths, "/home/u/.docker") {
		t.Errorf("~/.docker not rw for docker step: %v", rw.Paths)
	}
}

// TestComputeFSPolicy_DockerConfigDeniedForNonDocker keeps ~/.docker denied
// when the step is not a docker invocation, even if the dir is resolved.
func TestComputeFSPolicy_DockerConfigDeniedForNonDocker(t *testing.T) {
	spec := sandboxSpec{
		Repo:            "/home/u/proj",
		Cmdline:         "go test ./...",
		DockerConfigDir: "/home/u/.docker",
	}
	for _, a := range computeFSPolicy(spec, "/home/u/proj") {
		if containsPath(a.Paths, "/home/u/.docker") {
			t.Errorf("~/.docker granted as %s for non-docker step: %v", a.Kind, a.Paths)
		}
	}
}

// TestComputeFSPolicy_GitConfigIsROFile grants ~/.gitconfig as a per-FILE
// read grant ("rofile"), never as a directory ("ro"). A regular file in the
// dir bucket asks Landlock for read_dir/execute rights on a non-directory,
// which the kernel rejects ("inconsistent access rights"), failing the whole
// ruleset and silently degrading the sandbox to no-op.
func TestComputeFSPolicy_GitConfigIsROFile(t *testing.T) {
	spec := sandboxSpec{
		Repo:      "/home/u/proj",
		GitConfig: "/home/u/.gitconfig",
	}
	policy := computeFSPolicy(spec, "/home/u/proj")

	rofile := findAccess(policy, "rofile")
	if !containsPath(rofile.Paths, "/home/u/.gitconfig") {
		t.Errorf("~/.gitconfig not granted as rofile: %v", rofile.Paths)
	}
	ro := findAccess(policy, "ro")
	if containsPath(ro.Paths, "/home/u/.gitconfig") {
		t.Errorf("~/.gitconfig must NOT be in the ro (dir) bucket — kernel rejects dir rights on a file: %v", ro.Paths)
	}
	// Bare $HOME must still be denied.
	for _, a := range policy {
		if containsPath(a.Paths, "/home/u") {
			t.Errorf("$HOME granted as %s — must be denied: %v", a.Kind, a.Paths)
		}
	}
}

// TestComputeFSPolicy_Open returns no grants so the caller applies no
// Landlock ruleset (the escape hatch, mirroring network's precedent).
func TestComputeFSPolicy_Open(t *testing.T) {
	spec := sandboxSpec{Repo: "/home/u/proj", Filesystem: "open"}
	if got := computeFSPolicy(spec, "/home/u/proj"); got != nil {
		t.Errorf("filesystem: open must yield nil policy, got %v", got)
	}
}

// TestComputeFSPolicy_GoCachesRW grants GOMODCACHE + GOCACHE read-write
// (the Go toolchain writes them) and GOBIN read-only (execute-only is
// enough to run gotestsum/swag), while still denying bare $HOME.
func TestComputeFSPolicy_GoCachesRW(t *testing.T) {
	spec := sandboxSpec{
		Repo:       "/home/u/proj",
		GoModCache: "/home/u/go/pkg/mod",
		GoCache:    "/home/u/.cache/go-build",
		GoBin:      "/home/u/go/bin",
	}
	policy := computeFSPolicy(spec, "/home/u/proj")

	rw := findAccess(policy, "rw")
	if !containsPath(rw.Paths, "/home/u/go/pkg/mod") {
		t.Errorf("GOMODCACHE not rw: %v", rw.Paths)
	}
	if !containsPath(rw.Paths, "/home/u/.cache/go-build") {
		t.Errorf("GOCACHE not rw: %v", rw.Paths)
	}

	ro := findAccess(policy, "ro")
	if !containsPath(ro.Paths, "/home/u/go/bin") {
		t.Errorf("GOBIN not ro: %v", ro.Paths)
	}
	if containsPath(rw.Paths, "/home/u/go/bin") {
		t.Errorf("GOBIN must be ro, not rw: %v", rw.Paths)
	}

	// Bare $HOME must never be granted by any rule, even with the Go
	// subpaths opened: confinement still denies the rest of $HOME.
	for _, a := range policy {
		if containsPath(a.Paths, "/home/u") {
			t.Errorf("$HOME granted as %s — must be denied: %v", a.Kind, a.Paths)
		}
	}
}

// TestComputeFSPolicy_DevicesRW grants the standard writable device nodes
// a per-file read-write rule (so /dev stays ro but /dev/null is writable).
func TestComputeFSPolicy_DevicesRW(t *testing.T) {
	policy := computeFSPolicy(sandboxSpec{Repo: "/home/u/proj"}, "/home/u/proj")
	dev := findAccess(policy, "rwfile")
	for _, d := range []string{"/dev/null", "/dev/zero", "/dev/urandom", "/dev/ptmx"} {
		if !containsPath(dev.Paths, d) {
			t.Errorf("device %q not rwfile: %v", d, dev.Paths)
		}
	}
	// /dev itself stays read-only (the dir grant), only the nodes are rw.
	ro := findAccess(policy, "ro")
	if !containsPath(ro.Paths, "/dev") {
		t.Errorf("/dev should remain ro: %v", ro.Paths)
	}
}

// TestComputeFSPolicy_NoGoFieldsNoExtraGrant: an empty Go env (e.g. a
// non-Go step) adds no Go cache/bin grant beyond the baseline.
func TestComputeFSPolicy_NoGoFieldsNoExtraGrant(t *testing.T) {
	policy := computeFSPolicy(sandboxSpec{Repo: "/home/u/proj"}, "/home/u/proj")
	for _, a := range policy {
		for _, p := range a.Paths {
			if p == "/home/u/go/pkg/mod" || p == "/home/u/go/bin" || p == "/home/u/.cache/go-build" {
				t.Errorf("unexpected Go path %q granted as %s with empty Go fields", p, a.Kind)
			}
		}
	}
}

func TestResolveGoCacheDirs(t *testing.T) {
	tests := []struct {
		name                        string
		stepEnv                     map[string]string
		hostEnv                     []string
		wantMod, wantCache, wantBin string
	}{
		{
			name:    "defaults from HOME",
			hostEnv: []string{"HOME=/home/u"},
			// $GOPATH/pkg (not pkg/mod): the grant must also cover the
			// sibling pkg/sumdb the toolchain writes during go mod download.
			wantMod:   "/home/u/go/pkg",
			wantCache: "/home/u/.cache/go-build",
			wantBin:   "/home/u/go/bin",
		},
		{
			name:      "GOPATH overrides go root",
			hostEnv:   []string{"HOME=/home/u", "GOPATH=/srv/gopath"},
			wantMod:   "/srv/gopath/pkg",
			wantCache: "/home/u/.cache/go-build",
			wantBin:   "/srv/gopath/bin",
		},
		{
			name:      "explicit env wins",
			hostEnv:   []string{"HOME=/home/u"},
			stepEnv:   map[string]string{"GOMODCACHE": "/m", "GOCACHE": "/c", "GOBIN": "/b"},
			wantMod:   "/m",
			wantCache: "/c",
			wantBin:   "/b",
		},
		{
			name:      "step env wins over host env",
			hostEnv:   []string{"HOME=/home/u", "GOCACHE=/host"},
			stepEnv:   map[string]string{"GOCACHE": "/step"},
			wantMod:   "/home/u/go/pkg",
			wantCache: "/step",
			wantBin:   "/home/u/go/bin",
		},
		{
			name:      "no HOME no GOPATH yields empty",
			hostEnv:   nil,
			wantMod:   "",
			wantCache: "",
			wantBin:   "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotMod, gotCache, gotBin := resolveGoCacheDirs(tc.stepEnv, tc.hostEnv)
			if gotMod != tc.wantMod {
				t.Errorf("GOMODCACHE = %q, want %q", gotMod, tc.wantMod)
			}
			if gotCache != tc.wantCache {
				t.Errorf("GOCACHE = %q, want %q", gotCache, tc.wantCache)
			}
			if gotBin != tc.wantBin {
				t.Errorf("GOBIN = %q, want %q", gotBin, tc.wantBin)
			}
		})
	}
}

func TestCacheDirs(t *testing.T) {
	if got := cacheDirs(""); got != nil {
		t.Errorf("empty cache dir must yield nil, got %v", got)
	}
	got := cacheDirs("/c")
	if !reflect.DeepEqual(got, []string{"/c"}) {
		t.Errorf("cacheDirs = %v, want [/c]", got)
	}
}

func TestTmpdirFromEnv(t *testing.T) {
	if got := tmpdirFromEnv(map[string]string{"TMPDIR": "/a"}, nil); got != "/a" {
		t.Errorf("step env TMPDIR = %q, want /a", got)
	}
	if got := tmpdirFromEnv(nil, []string{"TMPDIR=/b"}); got != "/b" {
		t.Errorf("host env TMPDIR = %q, want /b", got)
	}
	// Step env wins over host env.
	if got := tmpdirFromEnv(map[string]string{"TMPDIR": "/a"}, []string{"TMPDIR=/b"}); got != "/a" {
		t.Errorf("step TMPDIR should win, got %q", got)
	}
	if got := tmpdirFromEnv(nil, nil); got != "" {
		t.Errorf("absent TMPDIR = %q, want empty", got)
	}
}

func TestDedupePaths(t *testing.T) {
	got := dedupePaths([]string{"/a", "", "/b", "/a", "/c", "/b"})
	want := []string{"/a", "/b", "/c"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dedupePaths = %v, want %v", got, want)
	}
}
