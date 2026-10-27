// Package layout is the single source of truth for the .sklp/ directory tree.
// It has no dependency on any other sklp package: pure path resolution.
package layout

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ErrStackNotFound: .sklp/<stack>/ directory does not exist.
var ErrStackNotFound = errors.New("stack not found")

// ErrNoContract: stack dir exists but <verb>.yaml is missing.
var ErrNoContract = errors.New("stack has no contract for this verb")

// pipelinesName / tasksName are the transverse CI dir and its legacy alias.
const (
	pipelinesName = "pipelines"
	tasksName     = "tasks"
)

// DefaultStack is the stack name a single-stack (mono-app) repo gets: a stack
// is always a folder, so a mono-app lives under .sklp/stack/. `sklp init` uses
// this; multi-stack repos name their own folders (core, transcript, …).
const DefaultStack = "stack"

// Layout resolves paths under a repo's .sklp/ tree.
type Layout struct{ Repo string }

// New builds a Layout for the given repo root.
func New(repo string) Layout { return Layout{Repo: repo} }

// root returns the absolute .sklp/ path.
func (l Layout) root() (string, error) {
	abs, err := filepath.Abs(l.Repo)
	if err != nil {
		return "", err
	}
	return filepath.Join(abs, ".sklp"), nil
}

// Stacks returns every stack folder under .sklp/ (pipelines/ and tasks/ excluded), sorted.
func (l Layout) Stacks() ([]string, error) {
	root, err := l.root()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if e.Name() == pipelinesName || e.Name() == tasksName {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// Verbs returns the contract verbs a stack exposes (the <verb>.yaml files present), sorted.
func (l Layout) Verbs(stack string) ([]string, error) {
	root, err := l.root()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(root, stack)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrStackNotFound, stack)
		}
		return nil, err
	}
	var verbs []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasSuffix(n, ".yaml") {
			verbs = append(verbs, strings.TrimSuffix(n, ".yaml"))
		}
	}
	sort.Strings(verbs)
	return verbs, nil
}

// StacksFor returns the stacks that expose a <verb>.yaml file, sorted.
func (l Layout) StacksFor(verb string) ([]string, error) {
	stacks, err := l.Stacks()
	if err != nil {
		return nil, err
	}
	root, err := l.root()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, s := range stacks {
		if st, err := os.Stat(filepath.Join(root, s, verb+".yaml")); err == nil && !st.IsDir() {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Resolve returns the absolute path to .sklp/<stack>/<verb>.yaml, or a typed error.
func (l Layout) Resolve(stack, verb string) (string, error) {
	root, err := l.root()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, stack)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return "", fmt.Errorf("%w: %s", ErrStackNotFound, stack)
	}
	path := filepath.Join(dir, verb+".yaml")
	if st, err := os.Stat(path); err == nil && !st.IsDir() {
		return path, nil
	}
	verbs, _ := l.Verbs(stack)
	if len(verbs) == 0 {
		return "", fmt.Errorf("%w: stack %q exposes no contracts", ErrNoContract, stack)
	}
	return "", fmt.Errorf("%w: stack %q exposes: %s", ErrNoContract, stack, strings.Join(verbs, ", "))
}

// PipelinesDir returns the resolved pipelines directory: canonical .sklp/pipelines,
// legacy .sklp/tasks fallback; canonical wins when both exist.
func (l Layout) PipelinesDir() string {
	abs, err := filepath.Abs(l.Repo)
	if err != nil {
		abs = l.Repo
	}
	canonical := filepath.Join(abs, ".sklp", pipelinesName)
	if st, err := os.Stat(canonical); err == nil && st.IsDir() {
		return canonical
	}
	legacy := filepath.Join(abs, ".sklp", tasksName)
	if st, err := os.Stat(legacy); err == nil && st.IsDir() {
		return legacy
	}
	return canonical
}

// Pipelines returns the runnable pipeline names (basename without extension,
// '_'-prefixed templates excluded) under the pipelines dir, sorted.
func (l Layout) Pipelines() ([]string, error) {
	dir := l.PipelinesDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasPrefix(n, "_") {
			continue
		}
		if !strings.HasSuffix(n, ".yaml") && !strings.HasSuffix(n, ".yml") {
			continue
		}
		names = append(names, strings.TrimSuffix(strings.TrimSuffix(n, ".yaml"), ".yml"))
	}
	sort.Strings(names)
	return names, nil
}
