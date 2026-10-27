package pipeline

import (
	"errors"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/internal/plan"
)

// fakeGit returns a gitRunner that maps a single-string command
// signature ("rev-parse HEAD") to a canned output. Any
// unrecognised invocation fails the test — that way a strategy that
// silently stops querying a placeholder is caught.
func fakeGit(t *testing.T, table map[string]string) gitRunner {
	t.Helper()
	return func(repoRoot string, args ...string) (string, error) {
		key := strings.Join(args, " ")
		v, ok := table[key]
		if !ok {
			return "", errors.New("unexpected git invocation: " + key)
		}
		return v, nil
	}
}

// fixedClock pins {{ date }} so the expectations below are exact. The real
// clock would make every assertion a moving target.
func fixedClock() time.Time {
	return time.Date(2026, 5, 16, 18, 56, 0, 0, time.UTC)
}

func canonicalFacts() map[string]string {
	return map[string]string{
		"rev-parse HEAD":              "bf44d9dabc1234567890",
		"rev-parse --abbrev-ref HEAD": "feat/foo",
	}
}

func TestResolveTag_Strategies(t *testing.T) {
	cases := []struct {
		name     string
		strategy string
		want     string
	}{
		{"date-sha", TagStrategyDateSha, "20260516-1856-bf44d9dabc12"},
		{"sha", TagStrategySha, "bf44d9dabc12"},
		{"date", TagStrategyDate, "20260516-1856"},
	}
	git := fakeGit(t, canonicalFacts())
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveTag(&TagBlock{Strategy: tc.strategy}, "/repo", git, fixedClock)
			if err != nil {
				t.Fatalf("resolveTag: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestResolveTag_Template(t *testing.T) {
	git := fakeGit(t, canonicalFacts())
	cases := []struct {
		template string
		want     string
	}{
		{"v{{ short_sha }}", "vbf44d9dabc12"},
		{"{{ date }}-{{ short_sha }}", "20260516-1856-bf44d9dabc12"},
		{"{{ full_sha }}", "bf44d9dabc1234567890"},
		{"branch-{{ branch }}", "branch-feat-foo"}, // slashes kebab-cased
		{"static", "static"},
		{"{{ unknown }}", "{{ unknown }}"}, // unknown placeholder left literal
	}
	for _, tc := range cases {
		t.Run(tc.template, func(t *testing.T) {
			got, err := resolveTag(&TagBlock{Template: tc.template}, "/repo", git, fixedClock)
			if err != nil {
				t.Fatalf("resolveTag: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestResolveTag_NilBlock(t *testing.T) {
	got, err := resolveTag(nil, "/repo", fakeGit(t, nil), fixedClock)
	if err != nil {
		t.Fatalf("resolveTag: %v", err)
	}
	if got != "" {
		t.Errorf("nil tag: got %q, want empty", got)
	}
}

func TestResolveTag_ConflictAndMissing(t *testing.T) {
	t.Run("both set", func(t *testing.T) {
		_, err := resolveTag(&TagBlock{Strategy: "sha", Template: "v{{ short_sha }}"}, "/repo", fakeGit(t, nil), fixedClock)
		if err == nil || !strings.Contains(err.Error(), "cannot set both") {
			t.Fatalf("want conflict error, got %v", err)
		}
	})
	t.Run("neither set", func(t *testing.T) {
		_, err := resolveTag(&TagBlock{}, "/repo", fakeGit(t, nil), fixedClock)
		if err == nil || !strings.Contains(err.Error(), "must set either") {
			t.Fatalf("want missing error, got %v", err)
		}
	})
	t.Run("unknown strategy", func(t *testing.T) {
		_, err := resolveTag(&TagBlock{Strategy: "semver"}, "/repo", fakeGit(t, canonicalFacts()), fixedClock)
		if err == nil || !strings.Contains(err.Error(), "unknown strategy") {
			t.Fatalf("want unknown-strategy error, got %v", err)
		}
	})
}

func TestSubstituteTagPlaceholders(t *testing.T) {
	facts := tagFacts{
		ShortSHA: "bf44d9d",
		FullSHA:  "bf44d9dabc",
		Date:     "20260516-1856",
		Branch:   "feat-foo",
	}
	in := "docker build -t img:{{ publish_tag }} --label sha={{ short_sha }} --label nope={{ unknown }}"
	want := "docker build -t img:20260516-1856-bf44d9d --label sha=bf44d9d --label nope={{ unknown }}"
	got := substituteTagPlaceholders(in, "20260516-1856-bf44d9d", facts)
	if got != want {
		t.Errorf("substitute:\n got=%q\nwant=%q", got, want)
	}
}

func TestSubstituteTagPlaceholders_NoBraces_FastPath(t *testing.T) {
	// No `{{` ⇒ exact same string back (covers the early return).
	in := "docker push img:static"
	out := substituteTagPlaceholders(in, "tag", tagFacts{})
	if out != in {
		t.Errorf("expected pass-through, got %q", out)
	}
}

// CompilePipeline-level tests ----------------------------------------

// Compile a tiny pipeline with a stub git runner by going through
// CompilePipeline's full code path. We can't inject a fake git from the
// public API today, so these tests use a real git repo in t.TempDir().
func TestCompilePipeline_NoTagBlock_BackwardCompat(t *testing.T) {
	// No Tag ⇒ no env, no substitution.
	p := Pipeline{
		Version: 1,
		Kind:    "pipeline",
		Steps: []Step{
			{Name: "echo", Run: "echo hi:{{ publish_tag }}"},
		},
	}
	compiled, err := CompilePipeline(p, CompilePipelineOptions{})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	step := compiled.CI.Steps[0]
	if step.Run != "echo hi:{{ publish_tag }}" {
		t.Errorf("Run should be untouched without tag block, got %q", step.Run)
	}
	if _, ok := step.Env[PublishTagEnvVar]; ok {
		t.Errorf("SKLP_PUBLISH_TAG must not be set when tag block is omitted")
	}
}

// tagTestRepo creates a fresh git repo with one commit so resolvePipelineTag
// has something deterministic to query. Skips if git is not on PATH.
func tagTestRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(cmd.Env,
			"HOME="+dir,
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "--initial-branch=main")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	run("commit", "--allow-empty", "-m", "init")
	return dir
}

func TestCompilePipeline_TagBlock_StrategyDateSha_E2E(t *testing.T) {
	repo := tagTestRepo(t)
	p := Pipeline{
		Version: 1,
		Kind:    "pipeline",
		Tag:     &TagBlock{Strategy: TagStrategyDateSha},
		Steps: []Step{{
			Name: "build",
			Run:  "docker build -t img:{{ publish_tag }} .",
			Env:  map[string]string{"MY_TAG": "{{ publish_tag }}"},
		}},
	}
	compiled, err := CompilePipeline(p, CompilePipelineOptions{RepoRoot: repo})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	step := compiled.CI.Steps[0]
	tagEnv, ok := step.Env[PublishTagEnvVar]
	if !ok || tagEnv == "" {
		t.Fatalf("SKLP_PUBLISH_TAG should be set; env=%v", step.Env)
	}
	if !strings.Contains(step.Run, "img:"+tagEnv+" ") {
		t.Errorf("Run did not substitute publish_tag: %q (want substring %q)", step.Run, "img:"+tagEnv)
	}
	if got := step.Env["MY_TAG"]; got != tagEnv {
		t.Errorf("MY_TAG=%q, want %q", got, tagEnv)
	}

	// Determinism: a second compile of the same input yields the same tag.
	compiled2, err := CompilePipeline(p, CompilePipelineOptions{RepoRoot: repo})
	if err != nil {
		t.Fatalf("compile (second): %v", err)
	}
	if compiled2.CI.Steps[0].Env[PublishTagEnvVar] != tagEnv {
		t.Errorf("tag is not deterministic: %q vs %q", compiled2.CI.Steps[0].Env[PublishTagEnvVar], tagEnv)
	}
}

// TestCompilePipeline_TagBlock_PublishFields_Substituted exercises
// the regression fixed by `fix(sklp): substitute publish_tag in
// docker-build step fields`. PR #411 added the structured docker-build
// fields (Image / Tag / Context / Dockerfile) AFTER PR #413 introduced
// the `{{ publish_tag }}` substitution pass, and the substitution walker
// was never extended to the new fields. A user passing
// `tag: "{{ publish_tag }}"` would see the literal placeholder reach
// `docker build -t ...:{{ publish_tag }}` and fail with "invalid
// reference format".
func TestCompilePipeline_TagBlock_PublishFields_Substituted(t *testing.T) {
	repo := tagTestRepo(t)
	push := false
	p := Pipeline{
		Version: 1,
		Kind:    "pipeline",
		Tag:     &TagBlock{Strategy: TagStrategyDateSha},
		Steps: []Step{{
			Name:       "build",
			Kind:       "publish",
			Image:      "registry.example/{{ branch }}/svc",
			Tag:        "{{ publish_tag }}",
			Context:    "ctx-{{ short_sha }}",
			Dockerfile: "ctx-{{ short_sha }}/Dockerfile.{{ branch }}",
			Push:       &push,
			Scan:       ScanConfigYAML{Present: true, Disabled: true},
		}},
	}
	compiled, err := CompilePipeline(p, CompilePipelineOptions{RepoRoot: repo})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	step := compiled.CI.Steps[0]
	if step.Publish == nil {
		t.Fatalf("Publish spec missing on compiled step")
	}
	tagEnv := step.Env[PublishTagEnvVar]
	if tagEnv == "" {
		t.Fatalf("SKLP_PUBLISH_TAG not set on step env")
	}
	if step.Publish.Tag != tagEnv {
		t.Errorf("Tag not substituted: got %q, want %q", step.Publish.Tag, tagEnv)
	}
	if strings.Contains(step.Publish.Image, "{{") {
		t.Errorf("Image not substituted: %q", step.Publish.Image)
	}
	if strings.Contains(step.Publish.Context, "{{") {
		t.Errorf("Context not substituted: %q", step.Publish.Context)
	}
	if strings.Contains(step.Publish.Dockerfile, "{{") {
		t.Errorf("Dockerfile not substituted: %q", step.Publish.Dockerfile)
	}
}

// TestCompilePipeline_NoTagBlock_PublishFields_Literal asserts the
// control: without a pipeline `tag:` block, no substitution happens and
// the literal `{{ publish_tag }}` is preserved (the existing "no tag
// block ⇒ literal" rule). Compile must surface the literal in the
// resolved PublishSpec.Tag exactly as written.
func TestCompilePipeline_NoTagBlock_PublishFields_Literal(t *testing.T) {
	push := false
	p := Pipeline{
		Version: 1,
		Kind:    "pipeline",
		Steps: []Step{{
			Name:    "build",
			Kind:    "publish",
			Image:   "registry.example/svc",
			Tag:     "{{ publish_tag }}",
			Context: "ctx",
			Push:    &push,
			Scan:    ScanConfigYAML{Present: true, Disabled: true},
		}},
	}
	compiled, err := CompilePipeline(p, CompilePipelineOptions{})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	step := compiled.CI.Steps[0]
	if step.Publish == nil {
		t.Fatalf("Publish spec missing on compiled step")
	}
	if step.Publish.Tag != "{{ publish_tag }}" {
		t.Errorf("Tag should stay literal without tag block, got %q", step.Publish.Tag)
	}
	if step.Publish.Image != "registry.example/svc" {
		t.Errorf("Image mutated unexpectedly: %q", step.Publish.Image)
	}
}

func TestCompilePipeline_TagBlock_BothSet_CompileError(t *testing.T) {
	p := Pipeline{
		Version: 1,
		Kind:    "pipeline",
		Tag:     &TagBlock{Strategy: "sha", Template: "v{{ short_sha }}"},
		Steps:   []Step{{Name: "echo", Run: "echo hi"}},
	}
	_, err := CompilePipeline(p, CompilePipelineOptions{RepoRoot: t.TempDir()})
	if err == nil {
		t.Fatal("expected compile error, got nil")
	}
	var ce *plan.CICompileError
	if !errors.As(err, &ce) {
		t.Fatalf("expected CICompileError, got %T: %v", err, err)
	}
	if !strings.Contains(ce.Reason, "cannot set both") {
		t.Errorf("want conflict reason, got %q", ce.Reason)
	}
}

// The regression this whole change exists for. HEAD's committer date is NOT the
// build order: ArgoCD's `build: automatic update` commits land on top of newer
// human commits while carrying an EARLIER timestamp. Tagging from %cd therefore
// minted tags that sorted below the tag already deployed, and Image Updater —
// which picks the highest tag alphabetically — never rolled them out. Three
// consecutive publishes shipped nothing before the cause was found.
//
// Two publishes from such a repo must still produce increasing tags.
func TestResolveTag_MonotonicAcrossAntedatedCommits(t *testing.T) {
	// A later publish whose HEAD is an antedated bot commit.
	botFacts := canonicalFacts()
	botFacts["rev-parse HEAD"] = "016bffc2dead1234567890"

	earlier, err := resolveTag(&TagBlock{Strategy: TagStrategyDateSha}, "/repo",
		fakeGit(t, canonicalFacts()), fixedClock)
	if err != nil {
		t.Fatalf("first publish: %v", err)
	}
	later, err := resolveTag(&TagBlock{Strategy: TagStrategyDateSha}, "/repo",
		fakeGit(t, botFacts), func() time.Time { return fixedClock().Add(time.Hour) })
	if err != nil {
		t.Fatalf("second publish: %v", err)
	}

	// Plain string comparison, because that is exactly how Image Updater picks.
	if !(later > earlier) {
		t.Errorf("a later publish must sort above an earlier one, got %q <= %q", later, earlier)
	}
}

// The date must be UTC. A local-time tag jumps backwards by an hour at the
// autumn DST change, reintroducing the regression above once a year, and two
// machines in different zones would mint tags that sort against each other.
func TestResolveTag_DateIsUTC(t *testing.T) {
	// 00:30 UTC — any zone east of UTC renders this as the PREVIOUS day, so a
	// local-time implementation fails on the date portion, not just the hour.
	midnightUTC := func() time.Time { return time.Date(2026, 5, 16, 0, 30, 0, 0, time.UTC) }
	east := time.FixedZone("UTC+5", 5*60*60)

	got, err := resolveTag(&TagBlock{Strategy: TagStrategyDate}, "/repo",
		fakeGit(t, canonicalFacts()), func() time.Time { return midnightUTC().In(east) })
	if err != nil {
		t.Fatalf("resolveTag: %v", err)
	}
	if got != "20260516-0030" {
		t.Errorf("got %q, want the UTC rendering 20260516-0030", got)
	}
}

// The tag width is a contract with ArgoCD Image Updater, not a free choice.
// Every allow-tags annotation in the infra repos pins `^[0-9]{8}-[0-9]{4}-...`,
// so a six-digit time is filtered out and never deploys. Worse, the two widths
// cannot coexist: alphabetical selection compares character by character, so
// `20260731-085134-x` sorts BELOW `20260731-1200-x` even though it is later.
// Widening this needs every annotation migrated in the same change.
func TestBuildDateLayout_MatchesImageUpdaterAllowTags(t *testing.T) {
	// The exact regexp deployed in the allow-tags annotations.
	allowTags := regexp.MustCompile(`^[0-9]{8}-[0-9]{4}-[0-9a-f]+$`)

	tag, err := resolveTag(&TagBlock{Strategy: TagStrategyDateSha}, "/repo",
		fakeGit(t, canonicalFacts()), fixedClock)
	if err != nil {
		t.Fatalf("resolveTag: %v", err)
	}
	if !allowTags.MatchString(tag) {
		t.Errorf("tag %q is rejected by the deployed allow-tags regexp — it would "+
			"never be rolled out; migrate every allow-tags annotation first", tag)
	}
}

// TestAbbrevSHA_FixedWidth locks the tag's SHA suffix to a constant width.
// It used to come from `git rev-parse --short HEAD`, which returns the
// shortest unambiguous prefix for the *local* object database: a full clone
// abbreviated to 9 chars while a shallow CI checkout gave 7. The same commit
// therefore published two different tags, and Image Updater redeployed
// identical code as if it were new.
func TestAbbrevSHA_FixedWidth(t *testing.T) {
	const full = "e810cffd5aa1b2c3d4e5f60718293a4b5c6d7e8f"
	if got := AbbrevSHA(full); got != full[:ShortSHALen] {
		t.Errorf("abbrevSHA = %q, want %q", got, full[:ShortSHALen])
	}
	if got := AbbrevSHA(full); len(got) != ShortSHALen {
		t.Errorf("len = %d, want %d", len(got), ShortSHALen)
	}
	short := "abc123"
	if got := AbbrevSHA(short); got != short {
		t.Errorf("AbbrevSHA(%q) = %q, want it returned as-is", short, got)
	}
}
