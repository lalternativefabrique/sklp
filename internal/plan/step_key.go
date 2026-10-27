package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// CIStepKeyInputs is the canonical, content-addressed input to a
// CI step's cache key (engine 2.3 — issue #386).
//
// Field set is deliberately frozen here so the JSON marshalling below
// produces a stable key across machines / sklp builds. Adding a field
// MUST come with a SchemaVersion bump (see plan.go) so older caches
// cannot be confused with newer ones.
type CIStepKeyInputs struct {
	// SchemaVersion mirrors plan.SchemaVersion so a schema bump
	// invalidates every previously cached per-step result.
	SchemaVersion int `json:"schema_version"`

	// Name is included so two steps with identical Run/Env (e.g. two
	// `echo hi` smoke steps) get distinct keys — otherwise a rename
	// without a content change would silently share a cache entry.
	Name string `json:"name"`

	// Run is the shell snippet to execute.
	Run string `json:"run"`

	// Cwd is the per-step working directory (reserved field on CIStep).
	Cwd string `json:"cwd,omitempty"`

	// Env is the resolved per-step env overlay, sorted by key for
	// deterministic JSON.
	Env []envPair `json:"env,omitempty"`

	// Tools is the per-step tool list, each paired with the
	// plan-level pinned version. Order matches CIStep.Tools so an
	// intentional reorder also invalidates the cache.
	Tools []toolPin `json:"tools,omitempty"`

	// InputsDigest is the Merkle-style hash of the step's resolved input
	// files (engine 2.4). For each file in sorted order we hash
	// (relative path, sha256 of content); the concatenation is then
	// hashed. Empty resolved set folds in the SHA-256 of an empty byte
	// slice so a step without declared inputs still has a stable key.
	InputsDigest string `json:"inputs_digest"`

	// UpstreamKey is the cache key of the step this one depends on.
	// In 2.3 we approximate "upstream" as the previous step in the
	// linear CIPlan.Steps order; 2.2 (DAG) will refine this to the
	// hash of the actual predecessor set. Empty for the first step.
	UpstreamKey string `json:"upstream_key"`

	// Network is the sandbox network policy ("" / "none" / ...).
	// Two steps that differ only in network isolation must not share
	// a cache entry — a "none" run cannot satisfy a host-network run.
	Network string `json:"network,omitempty"`

	// Filesystem is the sandbox FS policy ("" = confined / "open").
	// A confined run and an open run must not share a cache entry: the
	// confined run may have failed paths the open run would reach.
	Filesystem string `json:"filesystem,omitempty"`

	// Secrets is the per-step secret allowlist, sorted lexicographically
	// at build time so YAML ordering never affects the key.
	Secrets []string `json:"secrets,omitempty"`

	// DependsOn is the resolved predecessor name set, sorted
	// lexicographically at build time. UpstreamKey already folds in
	// predecessor content; this field guarantees that swapping which
	// declared dependency edges exist still invalidates the key even
	// if the upstream key collapses to the same value (e.g. single
	// predecessor, content-identical fan-in reorders).
	DependsOn []string `json:"depends_on,omitempty"`

	// StepKind discriminates the compiled step shape (empty for shell
	// steps, "publish" for the wrapper). Folded into the key so a
	// shell step that happens to share inputs with a publish step
	// can never collide on cache lookup.
	StepKind string `json:"step_kind,omitempty"`

	// Publish folds the publish configuration into the cache key when
	// this step is a publish wrapper. Composition rule:
	//   - Image:Tag literal — a different output ref must invalidate
	//     the cache even when the source tree is identical.
	//   - Dockerfile content + context inputs already enter the key
	//     via Run (we render the build command line) and
	//     InputsDigest (the resolved context inputs).
	//   - TrivyImage, ScanSeverity, ScanIgnoreUnfixed — a different
	//     scanner version or stricter severity must rescan.
	//   - Push/ScanEnabled flags — a previously cached
	//     build-only-no-push run is not a substitute for a build+push
	//     run; the flags discriminate so a flag flip rebuilds.
	Publish *publishKey `json:"docker_build,omitempty"`
}

// publishKey is the cache-key projection of PublishSpec.
// Mirrors the struct field-by-field so a future addition forces a
// SchemaVersion bump via the JSON shape change.
type publishKey struct {
	Image             string           `json:"image"`
	Tag               string           `json:"tag"`
	Context           string           `json:"context"`
	Dockerfile        string           `json:"dockerfile"`
	Push              bool             `json:"push"`
	ScanEnabled       bool             `json:"scan_enabled"`
	ScanSeverity      string           `json:"scan_severity,omitempty"`
	ScanIgnoreUnfixed bool             `json:"scan_ignore_unfixed"`
	TrivyImage        string           `json:"trivy_image"`
	Platform          string           `json:"platform"`
	BuildArgs         []envPair        `json:"build_args,omitempty"`
	BuildSecrets      []buildSecretKey `json:"build_secrets,omitempty"`
}

// buildSecretKey is the cache-key projection of one PublishSecret.
// We fold the (id, source kind, source ref) triple but never the
// resolved value: a build is deterministic with respect to its inputs,
// not with respect to the secret bytes. Rotating GITHUB_TOKEN must hit
// the cache; renaming the secret id from `github_token` to `gh_token`
// (or moving from env to file) must miss.
type buildSecretKey struct {
	ID         string `json:"id"`
	SourceKind string `json:"source_kind"`
	SourceRef  string `json:"source_ref"`
}

type envPair struct {
	K string `json:"k"`
	V string `json:"v"`
}

type toolPin struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// emptyInputsDigest is the sha256 of an empty byte slice; the value
// folded into a step's key when it declares no inputs.
var emptyInputsDigest = func() string {
	sum := sha256.Sum256(nil)
	return hex.EncodeToString(sum[:])
}()

// ComputeInputsDigest returns the Merkle-style digest of the supplied
// resolved input file list, read from repoRoot. Files are hashed in
// lexicographic order; each contribution is (relative path \x00 sha256
// of content \x00). A missing file is a hard error: the digest must
// reflect a real working tree state. An empty resolvedInputs list
// returns emptyInputsDigest.
func ComputeInputsDigest(repoRoot string, resolvedInputs []string) (string, error) {
	if len(resolvedInputs) == 0 {
		return emptyInputsDigest, nil
	}
	sorted := append([]string(nil), resolvedInputs...)
	sort.Strings(sorted)

	outer := sha256.New()
	for _, rel := range sorted {
		abs := filepath.Join(repoRoot, rel)
		f, err := os.Open(abs)
		if err != nil {
			return "", fmt.Errorf("open input %s: %w", rel, err)
		}
		inner := sha256.New()
		if _, err := io.Copy(inner, f); err != nil {
			f.Close()
			return "", fmt.Errorf("hash input %s: %w", rel, err)
		}
		f.Close()
		fmt.Fprintf(outer, "%s\x00%s\x00", rel, hex.EncodeToString(inner.Sum(nil)))
	}
	return hex.EncodeToString(outer.Sum(nil)), nil
}

// ComputeCIStepKey returns the content-addressed cache key for one
// compiled CI step. `toolVersions` is the plan-level pin map
// (Plan.CI.Tools); `upstreamKey` is the key of the previous step
// in declared order, or "" for the first step.
//
// The returned string is hex-encoded SHA-256 and safe to use as a
// directory name on every supported filesystem.
//
// NOTE: 2.3 derives "upstream" linearly. When 2.2 (DAG) lands, the
// caller should pass the hash of the actual dependency keys instead;
// the field name (UpstreamKey) is already DAG-friendly.
func ComputeCIStepKey(step CIStep, toolVersions map[string]string, upstreamKey, inputsDigest string) (string, error) {
	if inputsDigest == "" {
		inputsDigest = emptyInputsDigest
	}
	in := CIStepKeyInputs{
		SchemaVersion: SchemaVersion,
		Name:          step.Name,
		Run:           step.Run,
		Cwd:           step.Cwd,
		InputsDigest:  inputsDigest,
		UpstreamKey:   upstreamKey,
		Network:       step.Network,
		Filesystem:    step.Filesystem,
		StepKind:      string(step.StepKind),
	}
	if step.Publish != nil {
		dbk := &publishKey{
			Image:             step.Publish.Image,
			Tag:               step.Publish.Tag,
			Context:           step.Publish.Context,
			Dockerfile:        step.Publish.Dockerfile,
			Push:              step.Publish.Push,
			ScanEnabled:       step.Publish.ScanEnabled,
			ScanSeverity:      step.Publish.ScanSeverity,
			ScanIgnoreUnfixed: step.Publish.ScanIgnoreUnfixed,
			TrivyImage:        step.Publish.TrivyImage,
			Platform:          step.Publish.Platform,
		}
		if len(step.Publish.BuildArgs) > 0 {
			keys := make([]string, 0, len(step.Publish.BuildArgs))
			for k := range step.Publish.BuildArgs {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			dbk.BuildArgs = make([]envPair, 0, len(keys))
			for _, k := range keys {
				dbk.BuildArgs = append(dbk.BuildArgs, envPair{K: k, V: step.Publish.BuildArgs[k]})
			}
		}
		if len(step.Publish.BuildSecrets) > 0 {
			dbk.BuildSecrets = make([]buildSecretKey, 0, len(step.Publish.BuildSecrets))
			for _, sec := range step.Publish.BuildSecrets {
				kind, ref := "env", sec.Env
				if sec.File != "" {
					kind, ref = "file", sec.File
				}
				dbk.BuildSecrets = append(dbk.BuildSecrets, buildSecretKey{
					ID: sec.ID, SourceKind: kind, SourceRef: ref,
				})
			}
			sort.Slice(dbk.BuildSecrets, func(i, j int) bool {
				return dbk.BuildSecrets[i].ID < dbk.BuildSecrets[j].ID
			})
		}
		in.Publish = dbk
	}

	// Defensive normalisation: sort Secrets / DependsOn at the build
	// site so a YAML reorder never reshuffles the cache key. Copy first
	// to avoid mutating the caller's slices.
	if len(step.Secrets) > 0 {
		secrets := append([]string(nil), step.Secrets...)
		sort.Strings(secrets)
		in.Secrets = secrets
	}
	if len(step.DependsOn) > 0 {
		deps := append([]string(nil), step.DependsOn...)
		sort.Strings(deps)
		in.DependsOn = deps
	}

	if len(step.Env) > 0 {
		in.Env = make([]envPair, 0, len(step.Env))
		keys := make([]string, 0, len(step.Env))
		for k := range step.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			in.Env = append(in.Env, envPair{K: k, V: step.Env[k]})
		}
	}

	if len(step.Tools) > 0 {
		in.Tools = make([]toolPin, 0, len(step.Tools))
		for _, name := range step.Tools {
			in.Tools = append(in.Tools, toolPin{Name: name, Version: toolVersions[name]})
		}
	}

	body, err := json.Marshal(in)
	if err != nil {
		return "", fmt.Errorf("marshal step key inputs: %w", err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

// ComputeCIPlanKey returns a stable hash of the compiled CI plan as a
// whole. Used as the parent directory under .sklp/cache/runs/ so two
// independently-evolving pipelines in the same repo (e.g. ci.yaml vs
// publish.yaml) never share a per-step cache namespace.
//
// The hash covers every step's individual cache key plus the plan-level
// tool pin map. SchemaVersion is folded in via the per-step keys.
func ComputeCIPlanKey(p *Plan, repoRoot string) (string, error) {
	if p == nil || p.CI == nil {
		return "", fmt.Errorf("ComputeCIPlanKey: plan has no CI shape")
	}
	h := sha256.New()
	upstream := ""
	for _, st := range p.CI.Steps {
		digest, err := ComputeInputsDigest(repoRoot, st.ResolvedInputs)
		if err != nil {
			return "", fmt.Errorf("step %q inputs digest: %w", st.Name, err)
		}
		k, err := ComputeCIStepKey(st, p.CI.Tools, upstream, digest)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\x00", k)
		upstream = k
	}
	// Fold tool pin map for redundancy; per-step keys already include
	// the subset they reference, but this catches a "step does not list
	// the tool yet the plan changed the version" edge.
	if len(p.CI.Tools) > 0 {
		names := make([]string, 0, len(p.CI.Tools))
		for n := range p.CI.Tools {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Fprintf(h, "tool:%s=%s\x00", n, p.CI.Tools[n])
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
