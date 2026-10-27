package skillgen

import (
	"fmt"
	"strings"
)

// roles.go holds the role skills (dev / architect / ux-designer). They
// are adapted from the author's personal Claude skills, with every
// skalpai-specific constant (registry root, project id, hard-coded repo
// list) replaced by a value resolved from the scanned repo or a
// clearly-marked placeholder. The intent — TDD, DDD, the sklp workflow
// seam, issue-lifecycle discipline — is preserved verbatim, because that
// is the part that's worth shipping; only the constants are swapped.

// sklpWorkflowBlock is the shared "use sklp, not raw tooling" section.
// It's identical across roles, so it lives in one place and is spliced
// into each role body — same content the personal skills repeated.
func sklpWorkflowBlock(v Vars) string {
	var b strings.Builder
	b.WriteString("## sklp workflow\n\n")
	fmt.Fprintf(&b, "In %s, the `sklp` CLI owns the dev/CI/publish lifecycle. Use it as the source of truth — don't bypass it with raw test/build/docker commands for the standard pipelines.\n\n", v.ProjectName)
	b.WriteString("| Need | Command |\n")
	b.WriteString("|---|---|\n")
	b.WriteString("| Run CI locally before pushing | `sklp run ci` |\n")
	b.WriteString("| Build images (main only) | `sklp run publish` |\n")
	b.WriteString("| Local dev stack | `sklp dev` |\n")
	b.WriteString("| Diagnose a failing log | `sklp diagnose <file>` or `… 2>&1 \\| sklp diagnose -` |\n")
	b.WriteString("| Multi-pass investigation | `sklp investigate <file>` |\n\n")
	b.WriteString("Contracts live under `.sklp/`: `tasks/ci.yaml`, `tasks/publish.yaml`, `dev.yaml`, `space.yaml` (the closed-environment recipe: base image + toolchain + packages). Keep the YAML core minimal (`run` / `ready` / `depends_on`); new behavior goes into helpers/templates/codegen, not new top-level fields.")
	return b.String()
}

// issueLifecycleBlock is the shared issue-status discipline. Generic:
// no hard-coded project id (the personal skill pinned a project UUID —
// that's environment config resolved by the shell hook, not something to
// bake into a portable skill).
func issueLifecycleBlock() string {
	return strings.TrimSpace(`
## Issue tracking & lifecycle

File a ticket when a bug surfaces while implementing or reviewing — don't fix-and-forget:

` + "```bash" + `
sklp issue new "<title>" --type bug --body "<repro + observed + expected>"
sklp issue new "<title>" --type task   # for follow-ups
` + "```" + `

When the work references an existing issue, drive its status — don't leave tickets stale in backlog:

` + "```bash" + `
sklp issue status <id> in_progress   # on the first concrete action (first edit/commit)
sklp issue status <id> done          # only once merged / shipped
` + "```" + `

A pushed branch waiting on review is still ` + "`in_progress`" + `. Use ` + "`wontfix`" + ` for abandoned work. If the id is ambiguous, ask once — don't guess and flip the wrong ticket. No ticket referenced → no status calls.

The control plane for remote dispatch is the project's UI/runner, not GitHub Actions. Do not add ` + "`.github/workflows/*.yml`" + ` that drive ` + "`sklp run`" + `; local pre-push gates are git hooks (` + "`.githooks/pre-commit`" + `).`)
}

// devSkill — adapted from the personal "Development Skill". TDD,
// clean-code, error-handling intent preserved; the hard-coded repo
// allow-list ("skalpai, acme, …") is dropped in favour of "this
// repo".
func devSkill(v Vars) Skill {
	var b strings.Builder
	b.WriteString("# Development Skill\n\n")
	b.WriteString("Core implementation work: ship clean, tested, maintainable code that reads like the surrounding code.\n\n")
	b.WriteString("## Approach\n\n")
	b.WriteString("- Test-driven where it pays: write the failing test, make it pass, refactor.\n")
	b.WriteString("- Match the existing style — naming, error handling, comment density. Don't impose a new idiom.\n")
	b.WriteString("- Proper error handling and edge cases over happy-path-only code.\n")
	fmt.Fprintf(&b, "- This repo is %s (%s). Respect its layout and conventions.\n\n", v.ProjectName, v.langList())
	b.WriteString(sklpWorkflowBlock(v))
	b.WriteString("\n\n")
	b.WriteString(issueLifecycleBlock())
	return Skill{
		Name:        "dev",
		Description: fmt.Sprintf("Implement, debug and test features in %s following TDD and the sklp workflow.", v.ProjectName),
		Body:        b.String(),
	}
}

// architectSkill — adapted from the personal "Architect Skill". DDD /
// clean-architecture preferences kept; the container-registry policy is
// rewritten around the SCANNED registry root instead of the hard-coded
// skalpai one. A review NOTE is implied when the registry is a guess
// (the generator surfaces it separately).
func architectSkill(v Vars) Skill {
	var b strings.Builder
	b.WriteString("# Architect Skill\n\n")
	b.WriteString("System architecture and design: scalable, modular, documented solutions.\n\n")
	b.WriteString("## Preferences\n\n")
	b.WriteString("- Domain-Driven Design — bounded contexts, clear domain/application/infrastructure layering.\n")
	b.WriteString("- Clean Architecture — dependencies point inward; infrastructure is swappable.\n")
	b.WriteString("- Event-Driven where coupling hurts; documentation-first for non-trivial designs.\n\n")
	b.WriteString(sklpWorkflowBlock(v))
	b.WriteString("\n\n")
	b.WriteString("## Container registry policy\n\n")
	fmt.Fprintf(&b, "Images publish under a single registry root: `%s`.\n\n", v.Registry)
	b.WriteString("Recommended convention: `<registry-root>/<domain>/<service>:<immutable-tag>` where the domain groups by function (`platform`, `data`, `ai`, …) and the tag is immutable in production.\n\n")
	b.WriteString("Forbidden: `latest`, ambiguous names (`app`, `backend`, `service`), non-traceable tags (`final`, `v2-final`). Prefer date-sha tags (`YYYYMMDD-HHMM-<sha>`).\n\n")
	b.WriteString(issueLifecycleBlock())
	return Skill{
		Name:        "architect",
		Description: fmt.Sprintf("Design system architecture for %s using DDD/Clean Architecture and the sklp contract seam.", v.ProjectName),
		Body:        b.String(),
	}
}

// uxDesignerSkill — adapted from the personal "Sally" UX skill. The
// persona and method are project-agnostic already, so this is the
// lightest genericisation: only the project name is threaded in.
func uxDesignerSkill(v Vars) Skill {
	var b strings.Builder
	b.WriteString("# UX Designer — Sally 🎨\n\n")
	b.WriteString("You are **Sally**, a senior UX designer. Empathetic advocate with a storytelling flair: paint pictures with words and make the reader feel the user's problem.\n\n")
	b.WriteString("## Principles\n\n")
	b.WriteString("- Every decision serves a genuine user need, not aesthetics.\n")
	b.WriteString("- Start simple, evolve through feedback. Data-informed but always creative.\n")
	b.WriteString("- Cover the whole journey: errors, loading, empty states — not just the happy path.\n\n")
	b.WriteString("## Approach\n\n")
	fmt.Fprintf(&b, "1. **Understand context** — who are the users of %s, what are their goals, what constraints exist, what does success look like?\n", v.ProjectName)
	b.WriteString("2. **Tell user stories** — frame problems as narratives (\"Meet Sarah, who needs to…\").\n")
	b.WriteString("3. **Design with purpose** — user needs first, then edge cases, then aesthetics.\n")
	b.WriteString("4. **Deliver** — personas, journey maps, user flows, text wireframes, interaction patterns, accessibility (WCAG) notes.\n")
	return Skill{
		Name:        "ux-designer",
		Description: fmt.Sprintf("UX design, user research and interaction patterns for %s (persona: Sally).", v.ProjectName),
		Body:        b.String(),
	}
}
