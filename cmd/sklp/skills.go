package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/internal/layout"
	"github.com/lalternativefabrique/sklp/internal/repofacts"
	"github.com/lalternativefabrique/sklp/internal/skillgen"
	"github.com/lalternativefabrique/sklp/pipeline/reporter"
)

// skillsCommand declares `sklp skills` on the façade — generate AI-assistant
// skills (Claude, Cursor) tailored to this repo. Generation is DETERMINISTIC:
// a repofacts scan + embedded templates, no LLM. Output is diffable and safe to
// commit.
//
// Two families are written:
//   - lifecycle: init-dev, init-ci, init-config, init-publish
//   - roles:     dev, architect, ux-designer
//
// By default both Claude (.claude/skills/<name>/SKILL.md) and Cursor
// (.cursor/rules/<name>.mdc) targets are emitted. Existing files are skipped
// unless -force is passed, so re-running never silently clobbers hand-edited
// skills.
func skillsCommand() cli.Command {
	return cli.Command{
		Name:  "skills",
		Short: "Generate AI-assistant skills for this repo",
		Long: "Generate AI-assistant skills (Claude, Cursor) tailored to this repo.\n" +
			"Deterministic — no LLM, no API key needed.",
		Flags: []cli.Flag{
			{Name: "repo", Type: cli.StringFlag, StringDefault: ".", Usage: "repository root to inspect (made absolute)"},
			{Name: "target", Type: cli.StringFlag, StringDefault: "claude,cursor", Usage: "comma-separated targets: claude | cursor | codex | vibe | zed | cline"},
			{Name: "force", Type: cli.BoolFlag, Usage: "overwrite existing skill files"},
			{Name: "ascii", Type: cli.BoolFlag, Usage: "use ASCII fallback glyphs instead of UTF-8 symbols"},
			{Name: "print", Type: cli.StringFlag, Usage: "print the named skill for the first -target to stdout; writes nothing"},
		},
		Run: func(ctx context.Context, args cli.Args) int {
			repoAbs, err := filepath.Abs(args.String("repo"))
			if err != nil {
				fmt.Fprintf(os.Stderr, "sklp skills: repo abs: %v\n", err)
				return 1
			}
			if name := args.String("print"); name != "" {
				targets, err := parseTargets(args.String("target"))
				if err == nil {
					err = printSkill(os.Stdout, repoAbs, name, targets[0])
				}
				if err != nil {
					fmt.Fprintf(os.Stderr, "sklp skills: %v\n", err)
					return 1
				}
				return 0
			}
			targets, err := parseTargets(args.String("target"))
			if err != nil {
				fmt.Fprintf(os.Stderr, "sklp skills: %v\n", err)
				return 1
			}
			rep := reporter.New(os.Stderr, reporter.Options{ASCII: args.Bool("ascii")})
			if err := generateSkills(repoAbs, targets, args.Bool("force"), rep); err != nil {
				fmt.Fprintf(os.Stderr, "sklp skills: %v\n", err)
				return 1
			}
			return 0
		},
	}
}

// parseTargets turns the -target CSV into a validated, de-duplicated,
// order-stable slice of skillgen.Target. The special value "all"
// expands to every supported target.
func parseTargets(csv string) ([]skillgen.Target, error) {
	byName := map[string]skillgen.Target{
		"claude": skillgen.TargetClaude,
		"cursor": skillgen.TargetCursor,
		"codex":  skillgen.TargetCodex,
		"vibe":   skillgen.TargetVibe,
		"zed":    skillgen.TargetZed,
		"cline":  skillgen.TargetCline,
	}
	seen := map[skillgen.Target]bool{}
	var out []skillgen.Target
	add := func(target skillgen.Target) {
		if !seen[target] {
			seen[target] = true
			out = append(out, target)
		}
	}
	for _, raw := range strings.Split(csv, ",") {
		t := strings.TrimSpace(strings.ToLower(raw))
		if t == "" {
			continue
		}
		if t == "all" {
			for _, target := range skillgen.AllTargets {
				add(target)
			}
			continue
		}
		target, ok := byName[t]
		if !ok {
			return nil, fmt.Errorf("unknown target %q (want: claude, cursor, codex, vibe, zed, cline, all)", t)
		}
		add(target)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no valid target in %q", csv)
	}
	return out, nil
}

// generateSkills scans the repo, renders the skill set, and writes one
// file per (skill × target). Existing files are skipped unless force.
func generateSkills(repoAbs string, targets []skillgen.Target, force bool, rep reporter.Reporter) error {
	rep.Header("sklp skills")
	rep.SubAction("project    " + repoAbs)
	rep.SubAction("targets    " + targetNames(targets))

	rep.StepStarted("scanning project (deterministic)")
	start := time.Now()
	facts, err := repofacts.Scan(repoAbs)
	if err != nil {
		rep.StepFinished("scanning project (deterministic)", reporter.StatusFailed, time.Since(start), "")
		return fmt.Errorf("scan repo: %w", err)
	}
	rep.StepFinished("scanning project (deterministic)", reporter.StatusPassed, time.Since(start), "")

	vars := buildSkillVars(repoAbs, facts)
	skills := skillgen.Generate(vars)

	rep.StepStarted("writing skills")
	var written, skipped int
	emit := func(rel, contents string) error {
		abs := filepath.Join(repoAbs, rel)
		wrote, werr := writeSkillFile(abs, contents, force)
		if werr != nil {
			return fmt.Errorf("write %s: %w", rel, werr)
		}
		if wrote {
			written++
			rep.SubAction("wrote   " + rel)
		} else {
			skipped++
			rep.SubAction("skipped " + rel + " (exists; -force to overwrite)")
		}
		return nil
	}

	for _, t := range targets {
		if t.PerSkillFile() {
			for _, s := range skills {
				rel, contents := skillgen.Render(s, t)
				if err := emit(rel, contents); err != nil {
					rep.StepFinished("writing skills", reporter.StatusFailed, time.Since(start), "")
					return err
				}
			}
			continue
		}
		// Bundle targets (Zed) collapse every skill into one file.
		rel, contents := skillgen.RenderBundle(skills, t)
		if err := emit(rel, contents); err != nil {
			rep.StepFinished("writing skills", reporter.StatusFailed, time.Since(start), "")
			return err
		}
	}
	rep.StepFinished("writing skills", reporter.StatusPassed, time.Since(start), "")

	fields := []reporter.FinalField{
		{Label: "written", Value: fmt.Sprintf("%d files", written)},
		{Label: "skipped", Value: fmt.Sprintf("%d files", skipped)},
		{Label: "skills", Value: fmt.Sprintf("%d × %d targets", len(skills), len(targets))},
	}
	hints := []reporter.FinalField{
		{Label: "next", Value: "review the skills, then commit them"},
	}
	rep.Finalize("SKILLS", reporter.StatusPassed, time.Since(start), fields, hints)
	if !vars.RegistryInferred() {
		rep.SubAction("review: registry was not inferred — update the placeholder in the architect / publish skills")
	}
	return nil
}

// writeSkillFile writes contents to abs, creating parent dirs. Returns
// wrote=false (no error) when the file exists and force is off, so the
// caller can report a skip instead of clobbering a hand-edited skill.
func writeSkillFile(abs, contents string, force bool) (wrote bool, err error) {
	if !force {
		if _, statErr := os.Stat(abs); statErr == nil {
			return false, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return false, err
	}
	if err := os.WriteFile(abs, []byte(contents), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// devContractExists reports whether any stack under .sklp/ exposes a dev
// contract (.sklp/<stack>/dev.yaml). Stacks are folders, so this is true when
// at least one stack is dev-capable.
func devContractExists(repoAbs string) bool {
	stacks, err := layout.New(repoAbs).StacksFor("dev")
	return err == nil && len(stacks) > 0
}

// pipelineContractExists reports whether a pipeline file named `file`
// (e.g. "ci.yaml") exists under the repo's pipelines directory, honouring
// both the canonical `.sklp/pipelines/` and the legacy `.sklp/tasks/`.
func pipelineContractExists(repoAbs, file string) bool {
	for _, dir := range []string{"pipelines", "tasks"} {
		if _, err := os.Stat(filepath.Join(repoAbs, ".sklp", dir, file)); err == nil {
			return true
		}
	}
	return false
}

// projectName derives a display name for the repo: the dominant module
// name when there's a single one, else the root directory basename.
func projectName(repoAbs string, f *repofacts.RepoFacts) string {
	if f != nil && len(f.Languages) == 1 && len(f.Languages[0].Modules) == 1 {
		if n := f.Languages[0].Modules[0].Name; n != "" {
			// Use the last path segment of a module path like
			// github.com/org/repo so the name reads naturally.
			if i := strings.LastIndex(n, "/"); i >= 0 && i < len(n)-1 {
				return n[i+1:]
			}
			return n
		}
	}
	return filepath.Base(repoAbs)
}

func targetNames(targets []skillgen.Target) string {
	names := make([]string, len(targets))
	for i, t := range targets {
		names[i] = string(t)
	}
	return strings.Join(names, ", ")
}

func buildSkillVars(repoAbs string, facts *repofacts.RepoFacts) skillgen.Vars {
	return skillgen.BuildVars(
		facts,
		projectName(repoAbs, facts),
		pipelineContractExists(repoAbs, "ci.yaml"),
		pipelineContractExists(repoAbs, "publish.yaml"),
		devContractExists(repoAbs),
	)
}

func printSkill(w io.Writer, repoAbs, name string, target skillgen.Target) error {
	facts, err := repofacts.Scan(repoAbs)
	if err != nil {
		return fmt.Errorf("scan repo: %w", err)
	}
	for _, s := range skillgen.Generate(buildSkillVars(repoAbs, facts)) {
		if s.Name == name {
			_, contents := skillgen.Render(s, target)
			_, err := io.WriteString(w, contents)
			return err
		}
	}
	return fmt.Errorf("unknown skill %q", name)
}
