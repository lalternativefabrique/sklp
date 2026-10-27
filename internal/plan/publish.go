package plan

// publish.go — `kind: publish` step orchestrator (issue #411).
//
// A publish step is one CIStep as seen by the scheduler, the reporter, and
// the cache. It builds an image from a Dockerfile, scans it, and pushes it —
// fully daemonless: no Docker daemon, no BuildKit, no docker socket. The work
// lives in build_oci.go (NativeBuild): the Dockerfile is built to an OCI
// layout via the embedded runtime, scanned with `trivy image --input`, and
// pushed via the containerd resolver (PushOCILayout).
//
// The native builder is Linux-only (build_oci.go is //go:build linux); on
// other platforms nativeBuildFn keeps its loud-error default.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/lalternativefabrique/sklp/space/platform"
)

// InternalTrivyImage is the pinned Trivy image both scan paths use: the
// publish pipeline step and the daemonless `sklp image push --scan`. Exported
// so the two paths reuse the SAME pinned version — a divergent copy would let
// them enforce different vuln databases. Bumping it invalidates every cached
// publish step (carried into the cache key via PublishSpec.TrivyImage).
const InternalTrivyImage = "aquasec/trivy:0.50.4"

// internalTrivyImage is the unexported alias kept so the many in-package
// references stay untouched.
const internalTrivyImage = InternalTrivyImage

// defaultScanSeverity is the Trivy --severity filter applied when the
// user does not override it. HIGH+CRITICAL is the marquee default;
// users opt to a looser filter (e.g. CRITICAL only) per step.
var defaultScanSeverity = []string{"HIGH", "CRITICAL"}

// publishExec is the abstraction the orchestrator uses to invoke the scanner.
// Production code wires it to runSandboxed; tests inject a fake so they neither
// spawn a subprocess nor hit the network.
type publishExec func(ctx context.Context, name string, spec sandboxSpec) (int, error)

// nativeBuildFn runs a publish step with the daemonless Go builder. The default
// fails loudly: the native builder is Linux-only (build_oci.go, //go:build
// linux), where its init() replaces this with the real implementation. Declared
// here (always compiled) so both platform variants and runPublish see one
// symbol.
var nativeBuildFn = func(ctx context.Context, cfg CIExecConfig, step CIStep, secretDir string, stdoutW, stderrW io.Writer, execFn publishExec) (int, error) {
	fmt.Fprintln(stderrW, "[sklp publish] the native image builder is only available on Linux")
	return -1, errors.New("kind: publish unavailable on this platform (Linux only)")
}

// compilePublish validates a `kind: publish` step's raw fields and resolves
// them into a PublishSpec with defaults applied. The returned spec is what the
// executor and the cache key see.
func compilePublish(s CIStepInput) (*PublishSpec, error) {
	if s.Image == "" {
		return nil, errors.New("kind: publish requires `image:`")
	}
	if s.Tag == "" {
		return nil, errors.New("kind: publish requires `tag:`")
	}
	if s.Context == "" {
		return nil, errors.New("kind: publish requires `context:`")
	}
	if err := validateRelPaths([]string{s.Context}, "context"); err != nil {
		return nil, err
	}
	dockerfile := s.Dockerfile
	if dockerfile == "" {
		dockerfile = filepath.ToSlash(filepath.Join(s.Context, "Dockerfile"))
	} else if err := validateRelPaths([]string{dockerfile}, "dockerfile"); err != nil {
		return nil, err
	}

	push := true
	if s.Push != nil {
		push = *s.Push
	}

	// Defensive copies: keep the spec independent of the caller's maps /
	// slices so a later mutation in the runner doesn't leak through the
	// cache key.
	var buildArgs map[string]string
	if len(s.BuildArgs) > 0 {
		buildArgs = make(map[string]string, len(s.BuildArgs))
		for k, v := range s.BuildArgs {
			buildArgs[k] = v
		}
	}
	var buildSecrets []PublishSecret
	if len(s.BuildSecrets) > 0 {
		buildSecrets = append([]PublishSecret(nil), s.BuildSecrets...)
	}

	// 445: build_contexts — validate names + copy. The value side is opaque
	// (the builder parses it). Empty values are rejected at compile to surface
	// obvious typos early; the source format itself is left to the builder.
	var buildContexts map[string]string
	if len(s.BuildContexts) > 0 {
		buildContexts = make(map[string]string, len(s.BuildContexts))
		for k, v := range s.BuildContexts {
			if k == "" {
				return nil, errors.New("build_contexts: empty name (left side of the mapping)")
			}
			if v == "" {
				return nil, fmt.Errorf("build_contexts[%q]: empty source ref", k)
			}
			buildContexts[k] = v
		}
	}

	target, err := platform.Parse(s.Platform)
	if err != nil {
		return nil, fmt.Errorf("platform: %w", err)
	}

	spec := &PublishSpec{
		Platform:      target.String(),
		Image:         s.Image,
		Tag:           s.Tag,
		Context:       filepath.ToSlash(filepath.Clean(s.Context)),
		Dockerfile:    filepath.ToSlash(filepath.Clean(dockerfile)),
		Push:          push,
		TrivyImage:    internalTrivyImage,
		BuildArgs:     buildArgs,
		BuildSecrets:  buildSecrets,
		BuildContexts: buildContexts,
	}

	// Scan defaults: enabled with HIGH/CRITICAL + ignore_unfixed, unless
	// the user wrote `scan: false`.
	if s.ScanDisabled {
		spec.ScanEnabled = false
		return spec, nil
	}
	spec.ScanEnabled = true
	if len(s.ScanSeverity) > 0 {
		spec.ScanSeverity = strings.Join(s.ScanSeverity, ",")
	} else {
		spec.ScanSeverity = strings.Join(defaultScanSeverity, ",")
	}
	if s.ScanIgnoreUnfix != nil {
		spec.ScanIgnoreUnfixed = *s.ScanIgnoreUnfix
	} else {
		spec.ScanIgnoreUnfixed = true
	}
	return spec, nil
}

// runPublish executes one compiled publish step via the daemonless native
// builder (build_oci.go). stdoutW / stderrW receive the live output (and feed
// the cache capture via the tee in runStep); the return shape mirrors
// runSandboxed so runStep stays agnostic of the kind.
//
// The exec parameter is a seam: production calls pass sandboxedExec (a thin
// wrapper around runSandboxed); tests inject a fake.
func runPublish(ctx context.Context, cfg CIExecConfig, step CIStep, secretDir string, stdoutW, stderrW io.Writer, execFn publishExec) (int, error) {
	if step.Publish == nil {
		return -1, errors.New("publish: step has no Publish spec")
	}
	return nativeBuildFn(ctx, cfg, step, secretDir, stdoutW, stderrW, execFn)
}

// sandboxedExec is the default publishExec for production calls — a thin
// wrapper so the orchestrator stays decoupled from runSandboxed for tests.
func sandboxedExec(ctx context.Context, _ string, spec sandboxSpec) (int, error) {
	return runSandboxed(ctx, spec)
}

// failExit normalises the orchestrator's return value: a phase that exited
// non-zero is reported with that exit code; an internal error (couldn't even
// start) collapses to -1 so the surrounding runStep treats it as a hard
// failure.
func failExit(code int) int {
	if code <= 0 {
		return 1
	}
	return code
}

// shellQuote single-quotes a string for safe interpolation into a `sh -c`
// command line. Embedded single quotes are escaped via the classic '\” trick.
// Used by the native scan command builder (build_oci.go) which hands its line
// to sh -c.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n'\"\\$`*?[]{}!#<>|&;()") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// trivyFinding is a minimal slice of Trivy's JSON `Results[].Vulnerabilities`
// shape. Carried for tests that exercise CVE parsing.
type trivyFinding struct {
	VulnerabilityID  string `json:"VulnerabilityID"`
	PkgName          string `json:"PkgName"`
	InstalledVersion string `json:"InstalledVersion"`
	FixedVersion     string `json:"FixedVersion,omitempty"`
	Severity         string `json:"Severity"`
}

// parseTrivyJSON parses a Trivy JSON report into a flat findings list.
// Defensive: a malformed body returns no findings rather than an error so a
// transient Trivy output change never breaks the build pipeline purely from
// parsing.
func parseTrivyJSON(body []byte) []trivyFinding {
	type result struct {
		Vulnerabilities []trivyFinding `json:"Vulnerabilities"`
	}
	type doc struct {
		Results []result `json:"Results"`
	}
	var d doc
	if err := json.Unmarshal(body, &d); err != nil {
		return nil
	}
	var out []trivyFinding
	for _, r := range d.Results {
		out = append(out, r.Vulnerabilities...)
	}
	return out
}
