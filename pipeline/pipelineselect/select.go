package pipelineselect

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lalternativefabrique/sklp/pipeline/spec"
)

// SelectedPipeline is an auto-selected pipeline: its name and the YAML path to
// feed the runner.
type SelectedPipeline struct {
	Name string
	Path string
}

// SelectPipelines scans pipelinesDir for *.yaml pipelines whose `scopes:` match
// the changed-file set, returning them sorted by name. Pipelines without scopes
// are skipped: with nothing to match against, no diff can select them.
//
// Two branch filters apply, and they answer different questions. `on:` says
// where a push may START this pipeline — a pipeline opting out of the branch,
// or out of selection entirely, is skipped. `require.branches` says where it is
// ALLOWED to run: selecting a pipeline that require-time would refuse only
// produces a failed run, so it is skipped too.
//
// Reading a permission as an intent is what ran publish twice per merge:
// require.branches [main] made it eligible on exactly the branch where a merge
// also triggers it by name. `on:` is what a pipeline uses to say so itself.
//
// branch is the current git branch; "" disables both branch filters. Pure: no git.
func SelectPipelines(pipelinesDir, branch string, changed []string) ([]SelectedPipeline, error) {
	paths, err := filepath.Glob(filepath.Join(pipelinesDir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	var sel []SelectedPipeline
	for _, path := range paths {
		// Template files (_-prefixed) are not runnable; skip before loading.
		if strings.HasPrefix(filepath.Base(path), "_") {
			continue
		}
		p, err := spec.LoadPipeline(path)
		if err != nil {
			return nil, fmt.Errorf("load pipeline %s: %w", path, err)
		}
		if len(p.Scopes) == 0 {
			continue
		}
		if !spec.BranchAllowed(p.On, branch) {
			continue
		}
		if branch != "" && p.Require != nil && len(p.Require.Branches) > 0 &&
			!branchListed(p.Require.Branches, branch) {
			continue
		}
		match, err := ScopesMatch(p, changed)
		if err != nil {
			return nil, fmt.Errorf("match pipeline %s: %w", path, err)
		}
		if !match {
			continue
		}
		name := p.Name
		if name == "" {
			name = strings.TrimSuffix(filepath.Base(path), ".yaml")
		}
		sel = append(sel, SelectedPipeline{Name: name, Path: path})
	}
	sort.Slice(sel, func(i, j int) bool { return sel[i].Name < sel[j].Name })
	return sel, nil
}

// ScopesMatch reports whether any of p's `scopes:` cover the changed-file set.
//
// This is the scope question alone, deliberately separate from the trigger
// question the branch filters answer. A caller selecting pipelines for a push
// asks both; a caller starting one BY NAME — a merge into the default branch
// publishing, say — asks only this one, since the trigger is the merge itself.
// Conflating the two is what ran publish twice per merge in one direction, and
// would stop an `on: never` publish running at all in the other.
//
// With no scopes, or nothing changed, there is no gate and the answer is true —
// the underlying matcher prefers over-running to a silent skip. A caller that
// needs "unscoped" to mean something else checks len(p.Scopes) itself.
func ScopesMatch(p spec.Pipeline, changed []string) (bool, error) {
	names := make([]string, 0, len(p.Scopes))
	for name := range p.Scopes {
		names = append(names, name)
	}
	return spec.ScopeMatches(names, p.Scopes, changed)
}

// branchListed reports whether branch appears in list.
func branchListed(list []string, branch string) bool {
	for _, b := range list {
		if b == branch {
			return true
		}
	}
	return false
}
