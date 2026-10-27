// Package skillgen renders AI-assistant "skills" for a target repo.
//
// A skill is a Markdown brief that primes an AI coding assistant (Claude
// Code, Cursor, …) with the conventions of THIS repository: how to run
// CI, how to publish, what architecture style to respect, how the dev
// stack boots. sklp already owns that knowledge — the dev/CI/publish
// contracts under .sklp/, the deterministic RepoFacts scan — so it can
// emit the skills instead of every developer hand-writing them.
//
// Two families are produced:
//
//   - lifecycle skills (init-dev, init-ci, init-config, init-publish):
//     one per stage of the sklp lifecycle, teaching the assistant which
//     sklp verb owns that stage and the repo-specific commands around it.
//   - role skills (dev, architect, ux-designer): adapted from the
//     author's personal Claude skills, genericised so the constants that
//     were skalpai-specific (registry root, project id, repo list) become
//     values resolved from the scanned repo — or clearly-marked
//     placeholders when nothing could be inferred.
//
// Generation is DETERMINISTIC — no LLM. The same repo always yields
// byte-identical skills, so the output is diffable and safe to commit.
// Everything the templates need comes from repofacts.RepoFacts (already
// computed) plus a couple of cheap git lookups the caller passes in.
package skillgen

import (
	"fmt"
	"sort"
	"strings"

	"github.com/lalternativefabrique/sklp/internal/repofacts"
)

// Target is an AI-assistant flavour we know how to write files for.
// Each target decides the on-disk layout and the front-matter dialect;
// the Markdown body is shared across targets.
type Target string

const (
	// TargetClaude writes .claude/skills/<name>/SKILL.md with YAML
	// front-matter (name + description), matching Claude Code's skill
	// discovery convention.
	TargetClaude Target = "claude"

	// TargetCursor writes .cursor/rules/<name>.mdc with Cursor's
	// front-matter (description + globs + alwaysApply).
	TargetCursor Target = "cursor"

	// TargetCodex writes .agents/skills/<name>/SKILL.md — OpenAI Codex's
	// Agent Skill format. Same SKILL.md + name/description front-matter
	// as Claude; only the directory differs (.agents/skills vs
	// .claude/skills). See https://developers.openai.com/codex/skills.
	TargetCodex Target = "codex"

	// TargetVibe writes .vibe/skills/<name>/SKILL.md — Mistral Vibe's
	// skill layout under the .vibe/ config dir. Vibe shares the
	// SKILL.md + front-matter shape, so it reuses the Claude renderer.
	TargetVibe Target = "vibe"

	// TargetZed writes a single concatenated .rules file at the repo
	// root — Zed's primary rules file. Zed has no per-skill file/front-
	// matter concept, so every skill is appended as a "# <Title>"
	// section into one document.
	TargetZed Target = "zed"

	// TargetCline writes .clinerules/<NN>-<name>.md — Cline reads every
	// markdown file in .clinerules/ and merges them in alphanumeric
	// order, so we number the files to keep a stable order. Bodies are
	// raw markdown (no front-matter).
	TargetCline Target = "cline"
)

// AllTargets is the default fan-out when the caller doesn't restrict
// the target set. Order is stable for deterministic output.
var AllTargets = []Target{TargetClaude, TargetCursor, TargetCodex, TargetVibe, TargetZed, TargetCline}

// PerSkillFile reports whether a target writes ONE file per skill (the
// common case). Zed is the exception: it concatenates every skill into a
// single .rules file, so it is handled by RenderBundle instead of
// Render.
func (t Target) PerSkillFile() bool { return t != TargetZed }

// Skill is one rendered brief, target-agnostic. The renderer turns it
// into the right on-disk shape per Target.
type Skill struct {
	// Name is the kebab-case identifier ("dev", "init-ci"). Used as the
	// directory / file stem and as the front-matter `name`.
	Name string

	// Description is the one-line summary an assistant uses to decide
	// whether the skill is relevant to the current task.
	Description string

	// Body is the Markdown brief WITHOUT front-matter. The renderer
	// prepends the per-target front-matter.
	Body string

	// order is the skill's index in the generated set, used by the Cline
	// renderer to number files (NN-name.md) so the alphanumeric merge is
	// deterministic. Set by Generate; unexported because it's an internal
	// rendering detail, not part of the skill's identity.
	order int
}

// Vars holds the genericised values injected into every template. They
// are derived from the scanned repo so the emitted skills reference real
// commands and paths instead of placeholders wherever possible.
type Vars struct {
	// ProjectName is the repo's display name (basename of the root, or
	// the dominant module name). Drives "When working in <ProjectName>…".
	ProjectName string

	// Registry is the best-effort container registry root. Falls back to
	// a clearly-marked placeholder when the scan found nothing.
	Registry string

	// Languages is the sorted list of detected language short-names
	// ("go", "node", …). Drives stack-specific phrasing.
	Languages []string

	// HasCITask / HasPublishTask / HasDevConfig report whether the
	// corresponding .sklp contract already exists, so the lifecycle
	// skills can say "run it" vs "generate it first".
	HasCITask      bool
	HasPublishTask bool
	HasDevConfig   bool

	// registryInferred records whether Registry came from the scan
	// (true) or is a placeholder (false), so templates can emit a
	// review NOTE only when it's a guess.
	registryInferred bool
}

// BuildVars derives the template variables from scanned facts plus the
// caller-supplied existence flags for the .sklp contracts.
func BuildVars(f *repofacts.RepoFacts, projectName string, hasCI, hasPublish, hasDev bool) Vars {
	v := Vars{
		ProjectName:    projectName,
		HasCITask:      hasCI,
		HasPublishTask: hasPublish,
		HasDevConfig:   hasDev,
	}
	if f != nil {
		if f.Registry != "" {
			v.Registry = f.Registry
			v.registryInferred = true
		}
		for _, l := range f.Languages {
			v.Languages = append(v.Languages, l.Lang)
		}
		sort.Strings(v.Languages)
	}
	if v.Registry == "" {
		v.Registry = "rg.fr-par.scw.cloud/<your-namespace>"
	}
	if v.ProjectName == "" {
		v.ProjectName = "this repo"
	}
	return v
}

// RegistryInferred reports whether Registry came from the scan (true)
// or is a placeholder the operator must replace (false). The caller uses
// it to emit a review NOTE only when the registry is a guess.
func (v Vars) RegistryInferred() bool { return v.registryInferred }

// langList renders the detected languages as a human phrase
// ("Go, TypeScript and Rust") or a neutral fallback.
func (v Vars) langList() string {
	if len(v.Languages) == 0 {
		return "your stack"
	}
	pretty := make([]string, 0, len(v.Languages))
	for _, l := range v.Languages {
		pretty = append(pretty, prettyLang(l))
	}
	switch len(pretty) {
	case 1:
		return pretty[0]
	case 2:
		return pretty[0] + " and " + pretty[1]
	default:
		return strings.Join(pretty[:len(pretty)-1], ", ") + " and " + pretty[len(pretty)-1]
	}
}

func prettyLang(short string) string {
	switch short {
	case "go":
		return "Go"
	case "node":
		return "Node/TypeScript"
	case "rust":
		return "Rust"
	case "python":
		return "Python"
	case "java":
		return "Java"
	case "ruby":
		return "Ruby"
	case "php":
		return "PHP"
	case "dotnet":
		return ".NET"
	case "elixir":
		return "Elixir"
	default:
		return short
	}
}

// Generate renders the full set of skills for the given vars. The
// returned slice is sorted by Name for deterministic output.
func Generate(v Vars) []Skill {
	skills := []Skill{
		devSkill(v),
		architectSkill(v),
		uxDesignerSkill(v),
		initDevSkill(v),
		initCISkill(v),
		initConfigSkill(v),
		initPublishSkill(v),
		operatorSkill(v),
	}
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
	for i := range skills {
		skills[i].order = i + 1
	}
	return skills
}

// Render returns the on-disk relative path and file contents for a skill
// under a per-skill-file target. Path uses forward slashes; the caller
// joins it onto the repo root.
//
// Not valid for TargetZed (which bundles all skills into one file) — use
// RenderBundle for that. Calling Render with TargetZed falls through to
// the Claude layout, which the command layer avoids via PerSkillFile.
func Render(s Skill, t Target) (relPath, contents string) {
	switch t {
	case TargetCursor:
		return fmt.Sprintf(".cursor/rules/%s.mdc", s.Name), renderCursor(s)
	case TargetCodex:
		return fmt.Sprintf(".agents/skills/%s/SKILL.md", s.Name), renderClaude(s)
	case TargetVibe:
		return fmt.Sprintf(".vibe/skills/%s/SKILL.md", s.Name), renderClaude(s)
	case TargetCline:
		// Number the files so Cline's alphanumeric merge keeps a stable,
		// deterministic order. Index comes from the skill's position in
		// the generated set, injected via the Skill.order field.
		return fmt.Sprintf(".clinerules/%02d-%s.md", s.order, s.Name), renderPlain(s)
	default:
		return fmt.Sprintf(".claude/skills/%s/SKILL.md", s.Name), renderClaude(s)
	}
}

// RenderBundle concatenates every skill into a single document for
// targets that don't support per-skill files (Zed's .rules). The skills
// are emitted in slice order under "# <Title>" sections with a short
// header explaining the file is generated.
func RenderBundle(skills []Skill, t Target) (relPath, contents string) {
	var b strings.Builder
	b.WriteString("# Project AI rules\n\n")
	b.WriteString("Generated by `sklp skills`. Each section below is a skill: a role to adopt or a lifecycle stage to drive. Re-run `sklp skills -force` to refresh.\n")
	for _, s := range skills {
		b.WriteString("\n---\n\n")
		// The skill body already starts with its own "# Title"; keep it
		// so each section is self-describing.
		b.WriteString(strings.TrimRight(s.Body, "\n"))
		b.WriteString("\n")
	}
	return ".rules", b.String()
}

// renderClaude prepends Claude's YAML front-matter (name + description).
func renderClaude(s Skill) string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "name: %s\n", s.Name)
	fmt.Fprintf(&b, "description: %s\n", s.Description)
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimRight(s.Body, "\n"))
	b.WriteString("\n")
	return b.String()
}

// renderPlain emits the body as-is (no front-matter) for targets that
// merge raw markdown files — Cline's .clinerules/. The body already
// starts with a "# Title" heading, so the section is self-describing.
func renderPlain(s Skill) string {
	return strings.TrimRight(s.Body, "\n") + "\n"
}

// renderCursor prepends Cursor's .mdc front-matter. We mark role skills
// as on-demand (alwaysApply: false) — they're invoked by intent, not on
// every edit — and leave globs empty so the assistant matches on the
// description.
func renderCursor(s Skill) string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "description: %s\n", s.Description)
	b.WriteString("globs:\n")
	b.WriteString("alwaysApply: false\n")
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimRight(s.Body, "\n"))
	b.WriteString("\n")
	return b.String()
}
