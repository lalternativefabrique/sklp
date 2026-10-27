package reconcile

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseConfigReadsTargets(t *testing.T) {
	cfg, err := ParseConfig([]byte(`
interval: 30s
targets:
  - repo: https://github.com/example-org/shop.git
    branch: main
  - repo: https://github.com/acme-org/acme.git
    name: acme
`))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.Interval != 30*time.Second {
		t.Errorf("interval = %v, want 30s", cfg.Interval)
	}
	if len(cfg.Targets) != 2 {
		t.Fatalf("got %d targets, want 2", len(cfg.Targets))
	}
	if cfg.Targets[0].Name != "shop" {
		t.Errorf("name = %q, want it derived from the URL", cfg.Targets[0].Name)
	}
	if cfg.Targets[1].Branch != "main" {
		t.Errorf("branch = %q, want main by default", cfg.Targets[1].Branch)
	}
}

// A one-second poll hammers the forge and gains nothing: an image build takes
// minutes, so the floor is raised rather than honoured.
func TestParseConfigFloorsTheInterval(t *testing.T) {
	cfg, err := ParseConfig([]byte("interval: 1s\ntargets: []\n"))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.Interval < 10*time.Second {
		t.Fatalf("interval = %v, want the floor applied", cfg.Interval)
	}
}

func TestParseConfigDefaultsTheInterval(t *testing.T) {
	cfg, err := ParseConfig([]byte("targets: []\n"))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.Interval != DefaultInterval {
		t.Fatalf("interval = %v, want the default", cfg.Interval)
	}
}

// Two targets under one name would fight over the same mirror directory and
// over the loop's seen-refs map, so it is refused where it is written.
func TestParseConfigRejectsDuplicateNames(t *testing.T) {
	_, err := ParseConfig([]byte(`
targets:
  - repo: https://example.com/a/widgets.git
  - repo: https://other.example.com/b/widgets.git
`))
	if err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("err = %v, want a duplicate-name refusal", err)
	}
}

func TestParseConfigRejectsATargetWithNoRepo(t *testing.T) {
	if _, err := ParseConfig([]byte("targets:\n  - branch: main\n")); err == nil {
		t.Fatal("a target with no repo must be refused")
	}
}

// A missing file means this node pulls nothing — the correct state for one
// that only serves what an operator pushed with `sklp deploy`.
func TestLoadConfigTreatsAMissingFileAsNoTargets(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Targets) != 0 {
		t.Fatalf("got %d targets, want none", len(cfg.Targets))
	}
}

// The derived name reaches logs and a directory name, so a credential in the
// clone URL must not survive into it.
func TestRepoNameDropsCredentials(t *testing.T) {
	for remote, want := range map[string]string{
		"https://github.com/example-org/shop.git":            "shop",
		"https://user:token@github.com/example-org/shop.git": "shop",
		"git@github.com:acme-org/acme.git":                   "acme",
		"https://example.com/deep/path/widgets":              "widgets",
	} {
		got := RepoName(remote)
		if got != want {
			t.Errorf("RepoName(%q) = %q, want %q", remote, got, want)
		}
		if strings.Contains(got, "token") || strings.Contains(got, "@") {
			t.Errorf("RepoName(%q) leaked credentials: %q", remote, got)
		}
	}
}

func TestBuildTargetsMakesOneMirrorPerRepo(t *testing.T) {
	cache := t.TempDir()
	cfg, err := ParseConfig([]byte(`
targets:
  - repo: https://example.com/acme/one.git
  - repo: https://example.com/acme/two.git
    branch: release
`))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}

	targets, err := BuildTargets(cfg, cache)
	if err != nil {
		t.Fatalf("BuildTargets: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("got %d targets, want 2", len(targets))
	}
	if targets[0].Repo != "one" || targets[1].Branch != "release" {
		t.Fatalf("got %+v, want the config carried through", targets)
	}
	if targets[0].Source == nil {
		t.Error("target has no source to fetch from")
	}
	if MirrorDir(cache, "one") == MirrorDir(cache, "two") {
		t.Error("two repos share one mirror directory")
	}
}
