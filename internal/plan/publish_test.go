package plan

import (
	"runtime"
	"strings"
	"testing"
)

// TestCompilePublish_Defaults asserts the spec resolution applies
// the documented defaults: Dockerfile = <Context>/Dockerfile, push=true,
// scan enabled with HIGH/CRITICAL + ignore_unfixed=true, Trivy image
// pinned.
func TestCompilePublish_Defaults(t *testing.T) {
	in := CIStepInput{
		Name:    "build",
		Kind:    "publish",
		Image:   "registry.example.com/app",
		Tag:     "v1",
		Context: "apps/svc",
	}
	spec, err := compilePublish(in)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if spec.Dockerfile != "apps/svc/Dockerfile" {
		t.Errorf("Dockerfile = %q", spec.Dockerfile)
	}
	if !spec.Push {
		t.Error("Push default should be true")
	}
	if !spec.ScanEnabled {
		t.Error("ScanEnabled default should be true")
	}
	if spec.ScanSeverity != "HIGH,CRITICAL" {
		t.Errorf("ScanSeverity default = %q", spec.ScanSeverity)
	}
	if !spec.ScanIgnoreUnfixed {
		t.Error("ScanIgnoreUnfixed default should be true")
	}
	if spec.TrivyImage != internalTrivyImage {
		t.Errorf("TrivyImage = %q", spec.TrivyImage)
	}
}

// TestCompilePublish_Overrides walks the opt-out and override knobs:
// scan disabled, push disabled, custom severity / ignore_unfixed.
func TestCompilePublish_Overrides(t *testing.T) {
	f := false
	in := CIStepInput{
		Name:            "build",
		Kind:            "publish",
		Image:           "registry.example.com/app",
		Tag:             "v1",
		Context:         "apps/svc",
		Dockerfile:      "apps/svc/Dockerfile.alt",
		Push:            &f,
		ScanPresent:     true,
		ScanSeverity:    []string{"CRITICAL"},
		ScanIgnoreUnfix: &f,
	}
	spec, err := compilePublish(in)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if spec.Push {
		t.Error("Push should respect explicit false")
	}
	if spec.ScanSeverity != "CRITICAL" {
		t.Errorf("ScanSeverity = %q", spec.ScanSeverity)
	}
	if spec.ScanIgnoreUnfixed {
		t.Error("ScanIgnoreUnfixed should respect explicit false")
	}
	if spec.Dockerfile != "apps/svc/Dockerfile.alt" {
		t.Errorf("Dockerfile = %q", spec.Dockerfile)
	}
}

func TestCompilePublish_ScanDisabled(t *testing.T) {
	in := CIStepInput{
		Name:         "build",
		Kind:         "publish",
		Image:        "x/y",
		Tag:          "t",
		Context:      ".",
		ScanPresent:  true,
		ScanDisabled: true,
	}
	spec, err := compilePublish(in)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if spec.ScanEnabled {
		t.Error("scan: false should disable scanning")
	}
	if spec.ScanSeverity != "" {
		t.Errorf("disabled scan should have empty severity, got %q", spec.ScanSeverity)
	}
}

func TestCompilePublish_RequiredFields(t *testing.T) {
	cases := []struct {
		name string
		in   CIStepInput
		want string
	}{
		{"no image", CIStepInput{Kind: "publish", Tag: "t", Context: "c"}, "image"},
		{"no tag", CIStepInput{Kind: "publish", Image: "i", Context: "c"}, "tag"},
		{"no context", CIStepInput{Kind: "publish", Image: "i", Tag: "t"}, "context"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := compilePublish(tc.in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want contains %q", err, tc.want)
			}
		})
	}
}

// TestCompileCI_PublishStep exercises the integration: a CIStepInput
// with Kind=publish compiles to a CIStep that carries StepKind +
// Publish and no Run.
func TestCompileCI_PublishStep(t *testing.T) {
	in := CompileCIInput{
		Steps: []CIStepInput{{
			Name:    "build",
			Kind:    "publish",
			Image:   "x/y",
			Tag:     "v1",
			Context: "apps/svc",
		}},
	}
	pl, err := CompileCI(in, allMatch, nil)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	st := pl.CI.Steps[0]
	if st.StepKind != KindCIStepPublish {
		t.Errorf("StepKind = %q", st.StepKind)
	}
	if st.Publish == nil {
		t.Fatal("Publish spec missing")
	}
	if st.Run != "" {
		t.Errorf("publish step should have empty Run, got %q", st.Run)
	}
}

// TestCompileCI_PublishRejectsRun ensures a `kind: publish`
// step that ALSO declares a Run is rejected — the two shapes are
// mutually exclusive.
func TestCompileCI_PublishRejectsRun(t *testing.T) {
	in := CompileCIInput{
		Steps: []CIStepInput{{
			Name:    "build",
			Kind:    "publish",
			Image:   "x/y",
			Tag:     "v1",
			Context: ".",
			Run:     "echo nope",
		}},
	}
	_, err := CompileCI(in, allMatch, nil)
	if err == nil || !strings.Contains(err.Error(), "`run:`") {
		t.Fatalf("err = %v, want publish-step run rejection", err)
	}
}

func TestCompileCI_UnknownKind(t *testing.T) {
	in := CompileCIInput{
		Steps: []CIStepInput{{Name: "x", Kind: "nope", Run: "true"}},
	}
	_, err := CompileCI(in, allMatch, nil)
	if err == nil || !strings.Contains(err.Error(), "unknown step kind") {
		t.Fatalf("err = %v", err)
	}
}

// TestComputeCIStepKey_PublishSensitivity asserts that changing a
// publish dimension changes the cache key, and not changing it
// keeps the key stable.
func TestComputeCIStepKey_PublishSensitivity(t *testing.T) {
	base := CIStep{
		Name: "b", StepKind: KindCIStepPublish,
		Publish: &PublishSpec{
			Image: "x/y", Tag: "v1", Context: "ctx", Dockerfile: "ctx/Dockerfile",
			Push: true, ScanEnabled: true, ScanSeverity: "HIGH,CRITICAL",
			ScanIgnoreUnfixed: true, TrivyImage: internalTrivyImage,
		},
	}
	k1, err := ComputeCIStepKey(base, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	// Stable.
	k2, _ := ComputeCIStepKey(base, nil, "", "")
	if k1 != k2 {
		t.Error("key should be stable across identical specs")
	}
	// Tag change → different key.
	mut := base
	specCopy := *base.Publish
	specCopy.Tag = "v2"
	mut.Publish = &specCopy
	k3, _ := ComputeCIStepKey(mut, nil, "", "")
	if k3 == k1 {
		t.Error("tag change must invalidate cache key")
	}
	// Trivy version change → different key.
	mut2 := base
	specCopy2 := *base.Publish
	specCopy2.TrivyImage = "aquasec/trivy:0.99.0"
	mut2.Publish = &specCopy2
	k4, _ := ComputeCIStepKey(mut2, nil, "", "")
	if k4 == k1 {
		t.Error("Trivy version change must invalidate cache key")
	}
	// Severity change → different key.
	mut3 := base
	specCopy3 := *base.Publish
	specCopy3.ScanSeverity = "CRITICAL"
	mut3.Publish = &specCopy3
	k5, _ := ComputeCIStepKey(mut3, nil, "", "")
	if k5 == k1 {
		t.Error("severity change must invalidate cache key")
	}
	// Push flag change → different key.
	mut4 := base
	specCopy4 := *base.Publish
	specCopy4.Push = false
	mut4.Publish = &specCopy4
	k6, _ := ComputeCIStepKey(mut4, nil, "", "")
	if k6 == k1 {
		t.Error("push flag change must invalidate cache key")
	}
	// Shell-step variant with same Name must not collide.
	shell := CIStep{Name: "b", Run: "true"}
	k7, _ := ComputeCIStepKey(shell, nil, "", "")
	if k7 == k1 {
		t.Error("shell step must not collide with publish step")
	}
}

func TestParseTrivyJSON_Minimal(t *testing.T) {
	body := []byte(`{"Results":[{"Vulnerabilities":[{"VulnerabilityID":"CVE-1","PkgName":"openssl","InstalledVersion":"1.0","FixedVersion":"1.1","Severity":"HIGH"}]}]}`)
	out := parseTrivyJSON(body)
	if len(out) != 1 || out[0].VulnerabilityID != "CVE-1" {
		t.Errorf("got %+v", out)
	}
}

func TestParseTrivyJSON_Malformed(t *testing.T) {
	if out := parseTrivyJSON([]byte("not json")); out != nil {
		t.Errorf("malformed input should yield nil, got %+v", out)
	}
}

// TestComputeCIStepKey_BuildArgsSensitivity asserts build_args values
// affect the cache key (a different value invalidates), while reordering
// the map keys does not.
func TestComputeCIStepKey_BuildArgsSensitivity(t *testing.T) {
	mk := func(args map[string]string) CIStep {
		return CIStep{
			Name: "b", StepKind: KindCIStepPublish,
			Publish: &PublishSpec{
				Image: "x/y", Tag: "v1", Context: "ctx", Dockerfile: "ctx/Dockerfile",
				TrivyImage: internalTrivyImage, BuildArgs: args,
			},
		}
	}
	k1, _ := ComputeCIStepKey(mk(map[string]string{"A": "1", "B": "2"}), nil, "", "")
	k2, _ := ComputeCIStepKey(mk(map[string]string{"B": "2", "A": "1"}), nil, "", "")
	if k1 != k2 {
		t.Errorf("map order should not affect key: %s vs %s", k1, k2)
	}
	k3, _ := ComputeCIStepKey(mk(map[string]string{"A": "1", "B": "999"}), nil, "", "")
	if k3 == k1 {
		t.Errorf("changing a build_arg value must invalidate the key")
	}
}

// TestComputeCIStepKey_BuildSecretsCacheBehaviour pins the "rotate
// value, hit cache; rename id, miss cache" contract from issue #421.
//
// Cache key includes (id, source kind, source ref) — NOT the resolved
// value. The build is deterministic with respect to its inputs; the
// secret value is a runtime detail outside the input set, so rotating
// GITHUB_TOKEN must keep the previous build's result valid.
func TestComputeCIStepKey_BuildSecretsCacheBehaviour(t *testing.T) {
	mk := func(secs []PublishSecret) CIStep {
		return CIStep{
			Name: "b", StepKind: KindCIStepPublish,
			Publish: &PublishSpec{
				Image: "x/y", Tag: "v1", Context: "ctx", Dockerfile: "ctx/Dockerfile",
				TrivyImage: internalTrivyImage, BuildSecrets: secs,
			},
		}
	}
	base := mk([]PublishSecret{{ID: "github_token", Env: "GITHUB_TOKEN"}})
	k1, _ := ComputeCIStepKey(base, nil, "", "")

	// Same descriptor, different runtime env value → MUST equal (key
	// only folds env-var NAME, not value).
	k2, _ := ComputeCIStepKey(mk([]PublishSecret{{ID: "github_token", Env: "GITHUB_TOKEN"}}), nil, "", "")
	if k1 != k2 {
		t.Errorf("identical descriptor produced different keys")
	}

	// Renamed id → MUST differ.
	k3, _ := ComputeCIStepKey(mk([]PublishSecret{{ID: "gh_token", Env: "GITHUB_TOKEN"}}), nil, "", "")
	if k3 == k1 {
		t.Errorf("renaming secret id must invalidate the cache key")
	}

	// Same id, switched from env to file → MUST differ (source kind
	// dimension changed).
	k4, _ := ComputeCIStepKey(mk([]PublishSecret{{ID: "github_token", File: "/etc/tok"}}), nil, "", "")
	if k4 == k1 {
		t.Errorf("switching secret source kind must invalidate the cache key")
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"":           "''",
		"plain":      "plain",
		"with space": "'with space'",
		"a'b":        `'a'\''b'`,
		"x/y:tag":    "x/y:tag",
		"ctx":        "ctx",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCompilePublish_Platform(t *testing.T) {
	base := CIStepInput{Name: "b", Kind: "publish", Image: "r/app", Tag: "v1", Context: "."}

	spec, err := compilePublish(base)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if spec.Platform != "linux/"+runtime.GOARCH {
		t.Errorf("default Platform = %q", spec.Platform)
	}

	in := base
	in.Platform = "arm64"
	if spec, err = compilePublish(in); err != nil || spec.Platform != "linux/arm64" {
		t.Errorf("explicit platform: spec=%v err=%v", spec, err)
	}

	in.Platform = "windows/amd64"
	if _, err := compilePublish(in); err == nil || !strings.Contains(err.Error(), "platform") {
		t.Errorf("bad platform accepted: %v", err)
	}
}
