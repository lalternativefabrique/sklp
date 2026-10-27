package runnerimage

import "testing"

func baseRunner() Runner {
	return Runner{
		BaseImage: "rg.example/sklp-runner-base:latest",
		Toolchain: map[string]string{"go": "1.25.0", "node": "24.16.0"},
		Packages:  []string{"git", "curl"},
		GoTools:   []string{"gotest.tools/gotestsum@v1.13.0"},
		Env:       map[string]string{"GOPRIVATE": "github.com/digstack"},
	}
}

// TestRootfsHash_Stable: the same recipe hashes the same across calls, and the
// hash is the documented 12-hex-char width.
func TestRootfsHash_Stable(t *testing.T) {
	r := baseRunner()
	h1, err := RootfsHash(r)
	if err != nil {
		t.Fatalf("RootfsHash: %v", err)
	}
	h2, _ := RootfsHash(r)
	if h1 != h2 {
		t.Fatalf("hash not stable: %q != %q", h1, h2)
	}
	if len(h1) != 12 {
		t.Fatalf("hash width = %d, want 12 (%q)", len(h1), h1)
	}
}

// TestRootfsHash_SensitiveToRecipe: a change to any rendered field flips the
// hash. Map fields (toolchain/env) must hash deterministically despite map
// iteration order — renderDockerfile already sorts keys, this guards it.
func TestRootfsHash_SensitiveToRecipe(t *testing.T) {
	base, _ := RootfsHash(baseRunner())

	mutations := map[string]func(*Runner){
		"package added":   func(r *Runner) { r.Packages = append(r.Packages, "ffmpeg") },
		"toolchain bump":  func(r *Runner) { r.Toolchain["go"] = "1.26.0" },
		"go_tool added":   func(r *Runner) { r.GoTools = append(r.GoTools, "x/y@v1") },
		"base image swap": func(r *Runner) { r.BaseImage = "other:tag" },
		"env added":       func(r *Runner) { r.Env["FOO"] = "bar" },
	}
	for name, mut := range mutations {
		r := baseRunner()
		mut(&r)
		h, err := RootfsHash(r)
		if err != nil {
			t.Fatalf("%s: RootfsHash: %v", name, err)
		}
		if h == base {
			t.Errorf("%s: hash unchanged (%q) — recipe change not captured", name, h)
		}
	}
}

// TestRootfsHash_InsensitiveToMapOrder: building the same map with different
// insertion order yields the same hash (sortedKeys guarantee).
func TestRootfsHash_InsensitiveToMapOrder(t *testing.T) {
	r1 := baseRunner()
	r1.Env = map[string]string{"A": "1", "B": "2", "C": "3"}
	r2 := baseRunner()
	r2.Env = map[string]string{"C": "3", "A": "1", "B": "2"}
	h1, _ := RootfsHash(r1)
	h2, _ := RootfsHash(r2)
	if h1 != h2 {
		t.Fatalf("map insertion order changed the hash: %q != %q", h1, h2)
	}
}

// TestNeedsRunnerImage covers the detection rule: packages OR the explicit
// sentinel trigger the built rootfs; nothing else does (notably not secrets).
func TestNeedsRunnerImage(t *testing.T) {
	withPkgs := Runner{Packages: []string{"git"}}
	noPkgs := Runner{Secrets: []string{"GITHUB_TOKEN"}, GoTools: []string{"x/y@v1"}}

	cases := []struct {
		name string
		r    Runner
		want bool
	}{
		{"packages present", withPkgs, true},
		{"no packages", noPkgs, false},
		{"secrets alone do not trigger", Runner{Secrets: []string{"GITHUB_TOKEN"}}, false},
		{"go_tools alone do not trigger", Runner{GoTools: []string{"x/y@v1"}}, false},
	}
	for _, c := range cases {
		if got := NeedsRunnerImage(c.r); got != c.want {
			t.Errorf("%s: NeedsRunnerImage = %v, want %v", c.name, got, c.want)
		}
	}
}
