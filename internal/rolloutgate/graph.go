// Package rolloutgate orders image publication after the depends_on graph of
// the deploy manifests, and waits for a node to apply an image before the next
// one ships. Image watches roll each image on its own, so depends_on alone
// cannot keep a service from starting ahead of the job it needs.
package rolloutgate

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type target struct {
	space   string
	name    string
	job     bool
	repo    string
	deps    []string
	watched bool
}

type Graph struct {
	byRepo map[string]*target
	byName map[string]*target
}

type deployRoot struct {
	Deployments []struct {
		Space string `yaml:"space"`
		Path  string `yaml:"path"`
	} `yaml:"deployments"`
}

type manifestEntry struct {
	Image     string   `yaml:"image"`
	DependsOn []string `yaml:"depends_on"`
}

type manifest struct {
	Services map[string]manifestEntry `yaml:"services"`
	Jobs     map[string]manifestEntry `yaml:"jobs"`
}

type imageWatches struct {
	Watches []struct {
		ImageRepo string `yaml:"image_repo"`
	} `yaml:"watches"`
}

// Load reads the repo's deployments. A repo without .sklp/deploy-root.yaml has
// no graph: it returns nil, and publish keeps its declared order.
func Load(repo string) (*Graph, error) {
	var root deployRoot
	if err := readYAML(filepath.Join(repo, ".sklp", "deploy-root.yaml"), &root); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	g := &Graph{byRepo: map[string]*target{}, byName: map[string]*target{}}
	for _, d := range root.Deployments {
		var m manifest
		if err := readYAML(filepath.Join(repo, d.Path), &m); err != nil {
			return nil, err
		}
		g.add(d.Space, m.Services, false)
		g.add(d.Space, m.Jobs, true)
	}
	var w imageWatches
	if err := readYAML(filepath.Join(repo, ".sklp", "image-watches.yaml"), &w); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	for _, watch := range w.Watches {
		if t := g.byRepo[watch.ImageRepo]; t != nil {
			t.watched = true
		}
	}
	return g, nil
}

func (g *Graph) add(space string, entries map[string]manifestEntry, job bool) {
	for name, e := range entries {
		t := &target{space: space, name: name, job: job, repo: ImageRepo(e.Image), deps: e.DependsOn}
		g.byName[space+"/"+name] = t
		if t.repo != "" {
			g.byRepo[t.repo] = t
		}
	}
}

// Before reports whether the image repo first must be applied before second
// ships, following depends_on through services that are not being published.
func (g *Graph) Before(first, second string) bool {
	if g == nil {
		return false
	}
	a, b := g.byRepo[first], g.byRepo[second]
	if a == nil || b == nil || a == b || a.space != b.space {
		return false
	}
	seen := map[string]bool{}
	var reaches func(t *target) bool
	reaches = func(t *target) bool {
		for _, dep := range t.deps {
			key := t.space + "/" + dep
			if seen[key] {
				continue
			}
			seen[key] = true
			next := g.byName[key]
			if next == a || (next != nil && reaches(next)) {
				return true
			}
		}
		return false
	}
	return reaches(b)
}

// Watched reports whether a node rolls repo by itself. Waiting on anything else
// would hang: nothing would ever apply the pushed image.
func (g *Graph) Watched(repo string) bool {
	if g == nil {
		return false
	}
	t := g.byRepo[repo]
	return t != nil && t.watched
}

// Order sorts repos so each comes after those it depends on, keeping the
// declared order otherwise. It returns indexes into repos.
func (g *Graph) Order(repos []string) []int {
	done := make([]bool, len(repos))
	out := make([]int, 0, len(repos))
	ready := func(i int) bool {
		for j, r := range repos {
			if !done[j] && j != i && g.Before(r, repos[i]) {
				return false
			}
		}
		return true
	}
	for len(out) < len(repos) {
		next := -1
		for i := range repos {
			if !done[i] && ready(i) {
				next = i
				break
			}
		}
		if next < 0 {
			for i := range repos {
				if !done[i] {
					next = i
					break
				}
			}
		}
		done[next] = true
		out = append(out, next)
	}
	return out
}

// ImageRepo strips the tag or digest from an image reference.
func ImageRepo(image string) string {
	image, _, _ = strings.Cut(image, "@")
	slash := strings.LastIndex(image, "/")
	if colon := strings.LastIndex(image, ":"); colon > slash {
		return image[:colon]
	}
	return image
}

func readYAML(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(data, out); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}
