package skillgen

import (
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/internal/repofacts"
)

// sampleFacts is a small two-language repo with an inferred registry,
// covering the genericisation paths (registry, language list, name).
func sampleFacts() *repofacts.RepoFacts {
	return &repofacts.RepoFacts{
		Root:     "/tmp/acme",
		Registry: "ghcr.io/acme",
		Languages: []repofacts.LanguageFacts{
			{Lang: "go"},
			{Lang: "node"},
		},
	}
}

func TestGenerate_ProducesFullSet(t *testing.T) {
	v := BuildVars(sampleFacts(), "acme", true, false, true)
	got := Generate(v)

	want := map[string]bool{
		"dev": true, "architect": true, "ux-designer": true,
		"init-dev": true, "init-ci": true, "init-config": true, "init-publish": true,
		"sklp-operator": true,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d skills, want %d", len(got), len(want))
	}
	for _, s := range got {
		if !want[s.Name] {
			t.Errorf("unexpected skill %q", s.Name)
		}
		if s.Description == "" || s.Body == "" {
			t.Errorf("skill %q has empty description/body", s.Name)
		}
	}
}

func TestGenerate_Deterministic(t *testing.T) {
	v := BuildVars(sampleFacts(), "acme", true, true, true)
	a := Generate(v)
	b := Generate(v)
	if len(a) != len(b) {
		t.Fatalf("length differs: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("skill %d differs between runs:\n%+v\n%+v", i, a[i], b[i])
		}
	}
}

func TestGenerate_Genericisation(t *testing.T) {
	v := BuildVars(sampleFacts(), "acme", false, false, false)
	skills := byName(Generate(v))

	// Registry must be threaded into architect + init-publish.
	arch := skills["architect"].Body
	if !strings.Contains(arch, "ghcr.io/acme") {
		t.Errorf("architect body missing inferred registry:\n%s", arch)
	}
	// Project name must replace any hard-coded constant.
	if !strings.Contains(arch, "acme") {
		t.Errorf("architect body missing project name")
	}
	// No skalpai-specific constants must leak from the source skills.
	for name, s := range skills {
		for _, leak := range []string{"70d4811f", "skalpai-family", "acme, sporee"} {
			if strings.Contains(s.Body, leak) {
				t.Errorf("skill %q leaks skalpai-specific constant %q", name, leak)
			}
		}
	}
}

func TestGenerate_LifecycleStatusReflectsExistence(t *testing.T) {
	// CI present, publish absent → init-ci says "exists", init-publish "not yet".
	v := BuildVars(sampleFacts(), "acme", true, false, false)
	skills := byName(Generate(v))

	if !strings.Contains(skills["init-ci"].Body, "already exists") {
		t.Errorf("init-ci should report the contract as present")
	}
	if !strings.Contains(skills["init-publish"].Body, "not yet present") {
		t.Errorf("init-publish should report the contract as absent")
	}
}

func TestBuildVars_RegistryPlaceholderWhenMissing(t *testing.T) {
	f := &repofacts.RepoFacts{Root: "/tmp/x"}
	v := BuildVars(f, "x", false, false, false)
	if v.RegistryInferred() {
		t.Errorf("registry should not be marked inferred when absent")
	}
	if !strings.Contains(v.Registry, "your-namespace") {
		t.Errorf("expected placeholder registry, got %q", v.Registry)
	}

	v2 := BuildVars(sampleFacts(), "acme", false, false, false)
	if !v2.RegistryInferred() {
		t.Errorf("registry should be marked inferred when present")
	}
}

func TestRender_ClaudeFrontMatter(t *testing.T) {
	s := Skill{Name: "dev", Description: "do dev", Body: "# Dev\n\nbody"}
	path, contents := Render(s, TargetClaude)
	if path != ".claude/skills/dev/SKILL.md" {
		t.Errorf("claude path = %q", path)
	}
	if !strings.HasPrefix(contents, "---\nname: dev\ndescription: do dev\n---\n") {
		t.Errorf("claude front-matter wrong:\n%s", contents)
	}
	if !strings.HasSuffix(contents, "\n") {
		t.Errorf("file should end with newline")
	}
}

func TestRender_CursorFrontMatter(t *testing.T) {
	s := Skill{Name: "dev", Description: "do dev", Body: "# Dev"}
	path, contents := Render(s, TargetCursor)
	if path != ".cursor/rules/dev.mdc" {
		t.Errorf("cursor path = %q", path)
	}
	if !strings.Contains(contents, "alwaysApply: false") {
		t.Errorf("cursor front-matter missing alwaysApply:\n%s", contents)
	}
	if !strings.Contains(contents, "description: do dev") {
		t.Errorf("cursor front-matter missing description")
	}
}

func TestRender_CodexAndVibeReuseClaudeShape(t *testing.T) {
	s := Skill{Name: "dev", Description: "do dev", Body: "# Dev\n\nbody"}

	codexPath, codexContents := Render(s, TargetCodex)
	if codexPath != ".agents/skills/dev/SKILL.md" {
		t.Errorf("codex path = %q", codexPath)
	}
	if !strings.HasPrefix(codexContents, "---\nname: dev\n") {
		t.Errorf("codex should reuse SKILL.md front-matter:\n%s", codexContents)
	}

	vibePath, vibeContents := Render(s, TargetVibe)
	if vibePath != ".vibe/skills/dev/SKILL.md" {
		t.Errorf("vibe path = %q", vibePath)
	}
	if !strings.HasPrefix(vibeContents, "---\nname: dev\n") {
		t.Errorf("vibe should reuse SKILL.md front-matter")
	}
}

func TestRender_ClinePlainNumbered(t *testing.T) {
	s := Skill{Name: "architect", Description: "d", Body: "# Architect\n\nbody", order: 3}
	path, contents := Render(s, TargetCline)
	if path != ".clinerules/03-architect.md" {
		t.Errorf("cline path = %q, want numbered", path)
	}
	if strings.HasPrefix(contents, "---") {
		t.Errorf("cline files must be plain markdown, no front-matter:\n%s", contents)
	}
	if !strings.HasPrefix(contents, "# Architect") {
		t.Errorf("cline body should start with the skill heading")
	}
}

func TestRenderBundle_ZedSingleFile(t *testing.T) {
	v := BuildVars(sampleFacts(), "acme", true, true, true)
	skills := Generate(v)
	path, contents := RenderBundle(skills, TargetZed)
	if path != ".rules" {
		t.Errorf("zed bundle path = %q, want .rules", path)
	}
	// Every skill's heading should appear exactly once in the bundle.
	for _, s := range skills {
		title := firstHeading(s.Body)
		if title != "" && strings.Count(contents, title) != 1 {
			t.Errorf("zed bundle should contain %q exactly once", title)
		}
	}
	if TargetZed.PerSkillFile() {
		t.Errorf("Zed must not be a per-skill-file target")
	}
}

func firstHeading(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "# ") {
			return line
		}
	}
	return ""
}

func TestLangList(t *testing.T) {
	cases := map[string][]string{
		"your stack":                   nil,
		"Go":                           {"go"},
		"Go and Node/TypeScript":       {"go", "node"},
		"Go, Node/TypeScript and Rust": {"go", "node", "rust"},
	}
	for want, langs := range cases {
		v := Vars{Languages: langs}
		if got := v.langList(); got != want {
			t.Errorf("langList(%v) = %q, want %q", langs, got, want)
		}
	}
}

func byName(skills []Skill) map[string]Skill {
	m := make(map[string]Skill, len(skills))
	for _, s := range skills {
		m[s.Name] = s
	}
	return m
}
