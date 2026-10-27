package reconcile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// config.go declares what a node reconciles from.
//
// A webhook used to carry that: it named the repo, the branch and the identity
// to read them with. Pulling means the node has to know on its own, so it is
// stated in a file next to the control credentials rather than discovered —
// a node that cannot be reached inbound cannot be told, which is the whole
// point of ADR-014's pull.

// ConfigName is the file, resolved beside the node's other sklp config.
const ConfigName = "reconcile.yaml"

// DefaultInterval is how often a node polls when the file says nothing. A push
// then reconciles within a minute, the latency the pull model trades for
// needing no inbound path.
const DefaultInterval = time.Minute

// Config is a parsed reconcile.yaml.
type Config struct {
	// Interval between poll passes. Values under a floor are raised rather
	// than honoured: a one-second poll hammers the forge and gains nothing,
	// since an image build takes minutes.
	Interval time.Duration  `yaml:"interval"`
	Targets  []TargetConfig `yaml:"targets"`
}

// TargetConfig is one repo this node deploys from.
type TargetConfig struct {
	// Repo is the clone URL. A private repo carries its credential here, in a
	// file the node owns — there is no user session to borrow one from.
	Repo string `yaml:"repo"`
	// Branch defaults to main.
	Branch string `yaml:"branch"`
	// Name identifies the repo in logs and to the reconcilers. It defaults to
	// the URL's last path element, so the common case states nothing.
	Name string `yaml:"name"`
}

// minInterval floors the configured cadence.
const minInterval = 10 * time.Second

// LoadConfig reads and validates a reconcile.yaml.
//
// A missing file is not an error: it means this node reconciles nothing, which
// is the correct state for one that only serves what an operator pushed to it
// with `sklp deploy`. The caller reads an empty Targets as "no pull".
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &Config{Interval: DefaultInterval}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return ParseConfig(raw)
}

// ParseConfig decodes and validates the file's bytes.
func ParseConfig(raw []byte) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", ConfigName, err)
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.Interval < minInterval {
		cfg.Interval = minInterval
	}

	seen := map[string]bool{}
	for i := range cfg.Targets {
		t := &cfg.Targets[i]
		t.Repo = strings.TrimSpace(t.Repo)
		t.Branch = strings.TrimSpace(t.Branch)
		t.Name = strings.TrimSpace(t.Name)
		if t.Repo == "" {
			return nil, fmt.Errorf("%s: target %d declares no repo", ConfigName, i+1)
		}
		if t.Branch == "" {
			t.Branch = "main"
		}
		if t.Name == "" {
			t.Name = RepoName(t.Repo)
		}
		if seen[t.Name] {
			return nil, fmt.Errorf("%s: target %q is declared twice", ConfigName, t.Name)
		}
		seen[t.Name] = true
	}
	return &cfg, nil
}

// RepoName derives a short name from a clone URL, dropping any credential and
// the .git suffix — it reaches logs, so it must not carry a token.
func RepoName(remote string) string {
	name := remote
	if at := strings.LastIndex(name, "@"); at >= 0 {
		if scheme := strings.Index(name, "://"); scheme >= 0 {
			name = name[:scheme+3] + name[at+1:]
		}
	}
	name = strings.TrimSuffix(name, ".git")
	name = strings.TrimSuffix(name, "/")
	if slash := strings.LastIndex(name, "/"); slash >= 0 {
		name = name[slash+1:]
	}
	if colon := strings.LastIndex(name, ":"); colon >= 0 && !strings.Contains(name[colon+1:], "/") {
		name = name[colon+1:]
	}
	return name
}

// MirrorDir is where a repo's bare mirror lives under the node's cache.
func MirrorDir(cacheRoot, name string) string {
	return filepath.Join(cacheRoot, "reconcile", name+".git")
}
