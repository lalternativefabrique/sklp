// Package agentdetect figures out which AI coding assistant a developer
// uses, so `sklp init` can offer to install repo-tailored skills for it.
//
// Detection is two-signal and conservative:
//
//   - repo markers: a config dir/file the assistant writes into the repo
//     (.claude/, .cursor/, .agents/, .vibe/, .clinerules/, .rules).
//   - PATH binaries: the assistant's CLI being installed (claude, codex,
//     cursor-agent, …).
//
// A repo marker is a stronger signal than a PATH binary (it means the
// assistant is already used HERE), so detections are ranked with markers
// first. The caller presents the ranked list and lets the developer
// confirm or override — agentdetect never decides on its own.
package agentdetect

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	"github.com/lalternativefabrique/sklp/internal/skillgen"
)

// Agent is one known AI coding assistant and how to recognise it.
type Agent struct {
	// Target is the skillgen target used to write this agent's skills.
	Target skillgen.Target

	// Label is the human name shown in prompts ("Claude Code").
	Label string

	// repoMarkers are paths (relative to the repo root) whose presence
	// means the assistant is already configured in this repo.
	repoMarkers []string

	// binaries are executable names that, if found in PATH, mean the
	// assistant is installed on the machine.
	binaries []string

	// headlessArgs builds the argv (after the resolved binary) to run the
	// assistant non-interactively with a single prompt — so `sklp init`
	// can hand off to the developer's own agent and have it author the
	// .sklp contracts on the spot. Nil means "no known headless mode";
	// init then just installs the skills and tells the user to run them.
	headlessArgs func(prompt string) []string
}

// Binary returns the first of the agent's candidate executables that is
// on PATH, or "" when none is. Lets the caller decide whether a headless
// hand-off is even possible.
func (a Agent) Binary() string {
	for _, b := range a.binaries {
		if p, err := exec.LookPath(b); err == nil {
			return p
		}
	}
	return ""
}

// HeadlessCommand returns the binary + argv to run the agent
// non-interactively with prompt, and ok=false when the agent has no known
// headless mode or its binary isn't on PATH.
func (a Agent) HeadlessCommand(prompt string) (bin string, args []string, ok bool) {
	if a.headlessArgs == nil {
		return "", nil, false
	}
	bin = a.Binary()
	if bin == "" {
		return "", nil, false
	}
	return bin, a.headlessArgs(prompt), true
}

// catalog is the full set of assistants sklp can target. Order here is
// the tie-break order when two agents have the same detection strength.
var catalog = []Agent{
	{Target: skillgen.TargetClaude, Label: "Claude Code", repoMarkers: []string{".claude"}, binaries: []string{"claude"},
		// Claude Code runs non-interactively with `-p/--print <prompt>`.
		// `--verbose` makes it narrate each turn (tool calls, file writes)
		// to the terminal as it works, instead of staying silent until the
		// final result — so the hand-off shows live progress, not a multi-
		// minute freeze.
		headlessArgs: func(prompt string) []string { return []string{"-p", "--verbose", prompt} }},
	{Target: skillgen.TargetCodex, Label: "OpenAI Codex", repoMarkers: []string{".agents/skills", ".codex"}, binaries: []string{"codex"},
		// `codex exec <prompt>` runs Codex non-interactively (its docs:
		// "Run Codex non-interactively").
		headlessArgs: func(prompt string) []string { return []string{"exec", prompt} }},
	{Target: skillgen.TargetCursor, Label: "Cursor", repoMarkers: []string{".cursor"}, binaries: []string{"cursor-agent", "cursor"}},
	{Target: skillgen.TargetVibe, Label: "Mistral Vibe", repoMarkers: []string{".vibe"}, binaries: []string{"vibe"},
		// `vibe -p <prompt>` is Vibe's programmatic (auto-approve) mode.
		headlessArgs: func(prompt string) []string { return []string{"-p", prompt} }},
	{Target: skillgen.TargetZed, Label: "Zed", repoMarkers: []string{".rules", ".zed"}, binaries: []string{"zed"}},
	{Target: skillgen.TargetCline, Label: "Cline", repoMarkers: []string{".clinerules"}, binaries: []string{"cline"}},
}

// Detection is one agent that was found, with the evidence and a
// strength score for ranking (higher = more confident).
type Detection struct {
	Agent    Agent
	ViaRepo  bool // a repo marker matched
	ViaPath  bool // a PATH binary matched
	Strength int  // 2 = repo marker, 1 = PATH only
}

// Detect scans the repo at root and PATH, returning every recognised
// assistant ranked strongest-first. An empty slice means nothing was
// found — the caller should fall back to asking outright.
func Detect(root string) []Detection {
	var out []Detection
	for _, a := range catalog {
		viaRepo := anyExists(root, a.repoMarkers)
		viaPath := anyInPath(a.binaries)
		if !viaRepo && !viaPath {
			continue
		}
		strength := 1
		if viaRepo {
			strength = 2
		}
		out = append(out, Detection{Agent: a, ViaRepo: viaRepo, ViaPath: viaPath, Strength: strength})
	}
	// Strongest first; stable within equal strength by catalog order
	// (sort.SliceStable preserves the append order, which follows
	// catalog order).
	sort.SliceStable(out, func(i, j int) bool { return out[i].Strength > out[j].Strength })
	return out
}

// Catalog returns every known agent (for the "choose manually" prompt
// when detection found nothing).
func Catalog() []Agent { return catalog }

// AgentForTarget looks up the catalog entry for a skillgen target, so a
// caller holding only the chosen Target can reach the agent's headless
// invocation. ok=false for an unknown target.
func AgentForTarget(t skillgen.Target) (Agent, bool) {
	for _, a := range catalog {
		if a.Target == t {
			return a, true
		}
	}
	return Agent{}, false
}

func anyExists(root string, rels []string) bool {
	for _, rel := range rels {
		if _, err := os.Stat(filepath.Join(root, rel)); err == nil {
			return true
		}
	}
	return false
}

func anyInPath(bins []string) bool {
	for _, b := range bins {
		if _, err := exec.LookPath(b); err == nil {
			return true
		}
	}
	return false
}
