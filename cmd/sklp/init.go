package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/lalternativefabrique/sklp/internal/agentdetect"
	"github.com/lalternativefabrique/sklp/internal/cli"
	"github.com/lalternativefabrique/sklp/internal/layout"
	"github.com/lalternativefabrique/sklp/internal/repofacts"
	"github.com/lalternativefabrique/sklp/internal/skillgen"
	"github.com/lalternativefabrique/sklp/pipeline/reporter"
)

// initCommand declares `sklp init` — the onboarding verb.
//
// The flow is deterministic and LLM-free. It:
//
//  1. scans the repo (repofacts) — fast, reproducible.
//  2. detects which AI coding assistant the developer uses
//     (agentdetect: repo markers + PATH binaries) and, if one is found,
//     offers to install repo-tailored skills for it. The developer's
//     OWN assistant then authors the .sklp contracts, guided by the
//     lifecycle skills (init-dev / init-ci / init-config / init-publish).
//  3. if no assistant is available (or the developer declines), writes a
//     simple deterministic baseline: .sklp/config.yaml + dev.yaml +
//     tasks/ci.yaml.draft, from repofacts alone.
//
// There is no provider / model / API-key path anymore: sklp supplies
// facts and skills; the developer's assistant supplies the reasoning.
func initCommand() cli.Command {
	return cli.Command{
		Name:  "init",
		Short: "Onboard a repo: detect your AI assistant, install skills or a baseline",
		Long: "Onboard a repo onto sklp. Detects your AI coding assistant and offers to\n" +
			"install repo-tailored skills; otherwise writes a deterministic baseline.\n" +
			"No LLM, no API key.",
		Flags: []cli.Flag{
			{Name: "repo", Type: cli.StringFlag, StringDefault: ".", Usage: "repository root to inspect (made absolute)"},
			{Name: "force", Type: cli.BoolFlag, Usage: "overwrite existing files if present"},
			{Name: "yes", Type: cli.BoolFlag, Usage: "non-interactive: skip the agent prompt and write the deterministic baseline"},
			{Name: "agent", Type: cli.StringFlag, Usage: "skip detection and install skills for this assistant (claude|cursor|codex|vibe|zed|cline); 'none' forces the deterministic baseline"},
			{Name: "no-run", Type: cli.BoolFlag, Usage: "install skills but do NOT hand off to the agent; just print how to run them"},
			{Name: "ascii", Type: cli.BoolFlag, Usage: "use ASCII fallback glyphs instead of UTF-8 symbols"},
		},
		Run: func(ctx context.Context, args cli.Args) int {
			return runInit(args.String("repo"), args.String("agent"),
				args.Bool("force"), args.Bool("yes"), args.Bool("no-run"), args.Bool("ascii"))
		},
	}
}

// runInit is the init verb core. It returns the process exit code.
func runInit(repoFlag, agentFlag string, force, yes, noRun, ascii bool) int {
	repoAbs, err := filepath.Abs(repoFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sklp init: repo abs: %v\n", err)
		return 1
	}

	rep := reporter.New(os.Stderr, reporter.Options{ASCII: ascii})
	rep.Header("sklp init")
	rep.SubAction("project    " + repoAbs)

	// Resolve the chosen path: explicit -agent wins; else detect + ask;
	// else (in -yes mode) the baseline.
	chosen, useBaseline := resolveInitAgent(repoAbs, agentFlag, yes)

	if useBaseline {
		if err := writeBaseline(repoAbs, force, rep); err != nil {
			fmt.Fprintf(os.Stderr, "sklp init: %v\n", err)
			return 1
		}
		return 0
	}
	// Will we hand off to the agent afterwards? Only when not opted out AND
	// the agent actually has a reachable headless mode. This decides
	// whether installAgentSkills prints the "open <agent> yourself" hint
	// (no hand-off) or stays quiet and lets runAgentInit close the report.
	handOff := false
	if !noRun {
		if agent, ok := agentdetect.AgentForTarget(chosen); ok {
			if _, _, runnable := agent.HeadlessCommand(initHandoffPrompt); runnable {
				handOff = true
			}
		}
	}

	if err := installAgentSkills(repoAbs, chosen, force, handOff, rep); err != nil {
		fmt.Fprintf(os.Stderr, "sklp init: %v\n", err)
		return 1
	}

	if handOff {
		runAgentInit(repoAbs, chosen, yes, rep)
	}
	return 0
}

// initHandoffPrompt is what sklp feeds the agent in headless mode. It
// points at the installed skills and asks for the deterministic-but-
// reasoned contract generation we'd otherwise do by hand.
const initHandoffPrompt = "Use the init-config, init-dev and init-ci skills installed under this repo's " +
	"skills directory to generate the sklp contracts: .sklp/space.yaml, .sklp/stack/dev.yaml and " +
	".sklp/pipelines/ci.yaml. Read the repo to infer the real toolchain, services, database readiness " +
	"(prefer a log/ready probe for databases so dependents don't race a half-started server), env_files, " +
	"and migrate command. Validate with `sklp dev stack --validate` when done. Do not invent services that don't exist."

// runAgentInit invokes the chosen agent non-interactively on the repo so
// it generates the contracts itself. Best-effort: a missing headless mode
// or a declined confirmation falls back to the printed manual hint.
func runAgentInit(repoAbs string, target skillgen.Target, yes bool, rep reporter.Reporter) {
	agent, ok := agentdetect.AgentForTarget(target)
	if !ok {
		return
	}
	bin, args, ok := agent.HeadlessCommand(initHandoffPrompt)
	if !ok {
		// No headless mode / binary not on PATH — manual hint already shown.
		return
	}
	start := time.Now()
	if !yes && !confirm(fmt.Sprintf("Run %s now to generate the .sklp contracts?", agent.Label)) {
		finalizeHandoff(rep, agent.Label, start, reporter.StatusPassed,
			"declined — open "+agent.Label+" and run the init-* skills when ready")
		return
	}

	// Do NOT wrap the agent in a reporter spinner step: the agent narrates
	// to the terminal as it works (tool calls, file writes), and the
	// reporter's ~10 Hz bottom-line repaint would fight that stream and
	// leave the user staring at a frozen "running <agent>" line for
	// minutes. Print a plain banner instead and let the agent's own output
	// flow straight through, so progress is visible live.
	fmt.Fprintf(os.Stderr, "\nHanding off to %s — it will narrate below as it writes the contracts.\n", agent.Label)
	fmt.Fprintf(os.Stderr, "  $ %s %s\n\n", bin, strings.Join(args, " "))
	cmd := exec.Command(bin, args...)
	cmd.Dir = repoAbs
	cmd.Stdout = os.Stderr // the agent's own narration; keep stdout clean
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		finalizeHandoff(rep, agent.Label, start, reporter.StatusFailed,
			fmt.Sprintf("agent exited (%v) — review output above, or run the init-* skills manually", err))
		return
	}
	finalizeHandoff(rep, agent.Label, start, reporter.StatusPassed,
		"review the generated .sklp/* then `sklp dev stack --validate`")
}

// finalizeHandoff closes the init report after the agent hand-off path,
// which installAgentSkills deliberately left open.
func finalizeHandoff(rep reporter.Reporter, agentLabel string, start time.Time, status reporter.Status, hint string) {
	rep.Finalize("INIT", status, time.Since(start),
		[]reporter.FinalField{{Label: "agent", Value: agentLabel}},
		[]reporter.FinalField{{Label: "next", Value: hint}})
}

// resolveInitAgent decides whether to install skills for an assistant
// (returns its target + useBaseline=false) or to write the deterministic
// baseline (useBaseline=true). It honours -agent, then -yes, then falls
// back to interactive detection + confirmation.
func resolveInitAgent(repoAbs, agentFlag string, yes bool) (target skillgen.Target, useBaseline bool) {
	// Explicit override.
	if agentFlag != "" {
		if strings.EqualFold(agentFlag, "none") {
			return "", true
		}
		t, ok := targetForName(agentFlag)
		if !ok {
			exitf("sklp init: unknown -agent %q (want: claude, cursor, codex, vibe, zed, cline, none)", agentFlag)
		}
		return t, false
	}

	detections := agentdetect.Detect(repoAbs)

	// Non-interactive: never prompt. Use the strongest detection if any,
	// else the baseline.
	if yes {
		if len(detections) > 0 {
			return detections[0].Agent.Target, false
		}
		return "", true
	}

	switch len(detections) {
	case 0:
		// Nothing found — ask whether to pick one manually or go baseline.
		return askAgentManually()
	case 1:
		d := detections[0]
		if confirm(fmt.Sprintf("Detected %s (%s) — install its skills?", d.Agent.Label, evidence(d))) {
			return d.Agent.Target, false
		}
		return "", true
	default:
		return askAgentChoice(detections)
	}
}

// askAgentChoice prints the ranked detections and lets the developer
// pick one (or none).
func askAgentChoice(detections []agentdetect.Detection) (skillgen.Target, bool) {
	fmt.Fprintln(os.Stderr, "\nSeveral AI assistants detected:")
	for i, d := range detections {
		fmt.Fprintf(os.Stderr, "  %d) %s (%s)\n", i+1, d.Agent.Label, evidence(d))
	}
	fmt.Fprintln(os.Stderr, "  0) none — write the deterministic baseline instead")
	choice := prompt(fmt.Sprintf("Install skills for which? [1-%d / 0]", len(detections)))
	switch choice {
	case "", "0":
		return "", true
	default:
		idx := atoiSafe(choice)
		if idx >= 1 && idx <= len(detections) {
			return detections[idx-1].Agent.Target, false
		}
		fmt.Fprintln(os.Stderr, "unrecognised choice — writing the deterministic baseline")
		return "", true
	}
}

// askAgentManually runs when detection found nothing. It offers the full
// catalog so the developer can still install skills for an assistant
// sklp couldn't see, or fall through to the baseline.
func askAgentManually() (skillgen.Target, bool) {
	if !confirm("No AI assistant detected. Install skills for one anyway?") {
		return "", true
	}
	cat := agentdetect.Catalog()
	fmt.Fprintln(os.Stderr, "\nAvailable assistants:")
	for i, a := range cat {
		fmt.Fprintf(os.Stderr, "  %d) %s\n", i+1, a.Label)
	}
	choice := prompt(fmt.Sprintf("Which? [1-%d]", len(cat)))
	idx := atoiSafe(choice)
	if idx >= 1 && idx <= len(cat) {
		return cat[idx-1].Target, false
	}
	fmt.Fprintln(os.Stderr, "unrecognised choice — writing the deterministic baseline")
	return "", true
}

// installAgentSkills writes the skill set for the chosen assistant and
// points the developer at the next step: run their assistant to author
// the .sklp contracts using the freshly-installed lifecycle skills.
func installAgentSkills(repoAbs string, target skillgen.Target, force, willHandOff bool, rep reporter.Reporter) error {
	rep.SubAction("agent      " + agentLabel(target))
	rep.StepStarted("installing skills")
	start := time.Now()

	facts, err := repofacts.Scan(repoAbs)
	if err != nil {
		rep.StepFinished("installing skills", reporter.StatusFailed, time.Since(start), "")
		return fmt.Errorf("scan repo: %w", err)
	}
	vars := skillgen.BuildVars(
		facts,
		projectName(repoAbs, facts),
		pipelineContractExists(repoAbs, "ci.yaml"),
		pipelineContractExists(repoAbs, "publish.yaml"),
		devContractExists(repoAbs),
	)
	skills := skillgen.Generate(vars)

	var written, skipped int
	for _, s := range skills {
		rel, contents := skillgen.Render(s, target)
		wrote, werr := writeSkillFile(filepath.Join(repoAbs, rel), contents, force)
		if werr != nil {
			rep.StepFinished("installing skills", reporter.StatusFailed, time.Since(start), "")
			return fmt.Errorf("write %s: %w", rel, werr)
		}
		if wrote {
			written++
		} else {
			skipped++
		}
		rep.SubAction(skillStatus(wrote) + rel)
	}
	rep.StepFinished("installing skills", reporter.StatusPassed, time.Since(start), "")

	// When a hand-off to the agent will follow, don't finalize here with
	// the "open <agent> yourself" hint — runAgentInit owns the closing
	// report. Only finalize when this IS the end of the run.
	if willHandOff {
		return nil
	}

	fields := []reporter.FinalField{
		{Label: "agent", Value: agentLabel(target)},
		{Label: "skills", Value: fmt.Sprintf("%d written, %d skipped", written, skipped)},
	}
	hints := []reporter.FinalField{
		{Label: "next", Value: "open " + agentLabel(target) + " and ask it to run the init-* skills"},
		{Label: "or", Value: "sklp init -agent none   (deterministic baseline)"},
	}
	rep.Finalize("INIT", reporter.StatusPassed, time.Since(start), fields, hints)
	return nil
}

// writeBaseline writes the deterministic onboarding set with no LLM and
// no assistant: config.yaml (already deterministic), dev.yaml, and
// tasks/ci.yaml.draft. ci is a .draft so the operator reviews before it
// becomes the live contract; config + dev are written directly because
// `sklp dev stack --validate` / `sklp runner build` are the review step.
func writeBaseline(repoAbs string, force bool, rep reporter.Reporter) error {
	rep.SubAction("mode       deterministic baseline (no assistant)")
	rep.StepStarted("scanning project")
	start := time.Now()
	facts, err := repofacts.Scan(repoAbs)
	if err != nil {
		rep.StepFinished("scanning project", reporter.StatusFailed, time.Since(start), "")
		return fmt.Errorf("scan repo: %w", err)
	}
	rep.StepFinished("scanning project", reporter.StatusPassed, time.Since(start), "")

	type artifact struct {
		rel   string
		yaml  string
		notes []string
		draft bool
	}
	configYAML, configNotes := repofacts.RunnerConfig(facts)
	devYAML, devNotes := repofacts.DevConfig(repoAbs)
	ciYAML, ciNotes := repofacts.CIConfig(facts)
	arts := []artifact{
		{rel: filepath.Join(".sklp", "space.yaml"), yaml: configYAML, notes: configNotes},
		{rel: filepath.Join(".sklp", layout.DefaultStack, "dev.yaml"), yaml: devYAML, notes: devNotes},
		{rel: filepath.Join(".sklp", "pipelines", "ci.yaml.draft"), yaml: ciYAML, notes: ciNotes, draft: true},
	}

	rep.StepStarted("writing contracts")
	var reviewNotes []string
	for _, a := range arts {
		abs := filepath.Join(repoAbs, a.rel)
		if !force && a.draft {
			// Never clobber a real ci.yaml with a draft.
			if _, err := os.Stat(strings.TrimSuffix(abs, ".draft")); err == nil {
				rep.SubAction("skipped " + a.rel + " (real contract exists)")
				continue
			}
		}
		if err := writeDraft(abs, []byte(a.yaml), force); err != nil {
			rep.StepFinished("writing contracts", reporter.StatusFailed, time.Since(start), "")
			return fmt.Errorf("write %s: %w", a.rel, err)
		}
		rep.SubAction("wrote   " + a.rel)
		for _, n := range a.notes {
			reviewNotes = append(reviewNotes, a.rel+": "+n)
		}
	}
	rep.StepFinished("writing contracts", reporter.StatusPassed, time.Since(start), "")

	hints := []reporter.FinalField{
		{Label: "next", Value: "review · sklp dev stack --validate · sklp run ci"},
		{Label: "tip", Value: "rename pipelines/ci.yaml.draft → ci.yaml when ready"},
	}
	rep.Finalize("INIT", reporter.StatusPassed, time.Since(start), nil, hints)
	for _, n := range reviewNotes {
		rep.SubAction("review: " + n)
	}
	return nil
}

// --- small helpers -------------------------------------------------------

func targetForName(name string) (skillgen.Target, bool) {
	switch strings.ToLower(name) {
	case "claude":
		return skillgen.TargetClaude, true
	case "cursor":
		return skillgen.TargetCursor, true
	case "codex":
		return skillgen.TargetCodex, true
	case "vibe":
		return skillgen.TargetVibe, true
	case "zed":
		return skillgen.TargetZed, true
	case "cline":
		return skillgen.TargetCline, true
	default:
		return "", false
	}
}

func agentLabel(t skillgen.Target) string {
	for _, a := range agentdetect.Catalog() {
		if a.Target == t {
			return a.Label
		}
	}
	return string(t)
}

func evidence(d agentdetect.Detection) string {
	switch {
	case d.ViaRepo && d.ViaPath:
		return "config + CLI"
	case d.ViaRepo:
		return "repo config"
	default:
		return "CLI in PATH"
	}
}

func skillStatus(wrote bool) string {
	if wrote {
		return "wrote   "
	}
	return "skipped "
}

var stdinReader = bufio.NewReader(os.Stdin)

// prompt writes a question to stderr and reads one trimmed line.
func prompt(q string) string {
	fmt.Fprintf(os.Stderr, "\n%s ", q)
	line, err := stdinReader.ReadString('\n')
	if err != nil {
		return ""
	}
	return strings.TrimSpace(line)
}

// confirm asks a yes/no question defaulting to yes ([Y/n]).
func confirm(q string) bool {
	ans := strings.ToLower(prompt(q + " [Y/n]"))
	return ans == "" || ans == "y" || ans == "yes"
}

func atoiSafe(s string) int {
	if s == "" {
		return -1
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return -1
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// writeDraft creates the output file with mode 0644, making parent dirs
// as needed. Shared with `sklp skills`. force controls overwrite.
func writeDraft(path string, data []byte, force bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	flag := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if !force {
		flag |= os.O_EXCL
	}
	f, err := os.OpenFile(path, flag, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s already exists (use -force)", path)
		}
		return err
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		if _, err := f.Write([]byte("\n")); err != nil {
			return err
		}
	}
	return nil
}

// exitf prints to stderr and exits 1. Shared across the init/skills verbs.
func exitf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
