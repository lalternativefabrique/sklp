package runnerimage

import (
	"fmt"
	"strings"
)

// Instruction is one parsed line of a RENDERED runner Dockerfile. The renderer
// (renderFromBase) emits a deliberately restricted grammar — one instruction
// per line, no line continuations, no COPY — so this is a tiny line parser, NOT
// a general Dockerfile parser. The Go builder consumes these instead of shelling
// out to `docker build`.
type Instruction struct {
	Op    string // FROM | RUN | ENV | WORKDIR
	Value string // the argument text after the op (raw, for RUN/FROM/WORKDIR)
	Key   string // ENV only: the variable name
	Env   string // ENV only: the variable value
}

// ParseRunnerDockerfile parses a rendered runner Dockerfile into ordered
// instructions. It rejects anything outside the runner grammar (notably COPY /
// ADD / multi-stage) so a hand-edited or future-rendered file that needs a real
// builder fails loudly here instead of silently producing a wrong rootfs.
func ParseRunnerDockerfile(dockerfile string) ([]Instruction, error) {
	var out []Instruction
	for i, raw := range strings.Split(dockerfile, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		op, rest, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("line %d: %q has no argument", i+1, line)
		}
		rest = strings.TrimSpace(rest)
		switch op {
		case "FROM", "RUN", "WORKDIR":
			out = append(out, Instruction{Op: op, Value: rest})
		case "ENV":
			// Renderer emits exactly `ENV KEY=value` (single pair per line).
			k, v, ok := strings.Cut(rest, "=")
			if !ok {
				return nil, fmt.Errorf("line %d: ENV %q is not KEY=value", i+1, rest)
			}
			out = append(out, Instruction{Op: "ENV", Key: k, Env: v})
		default:
			return nil, fmt.Errorf("line %d: unsupported instruction %q — the Go runner builder handles only the generated runner grammar (FROM/RUN/ENV/WORKDIR), not %s", i+1, op, op)
		}
	}
	if len(out) == 0 || out[0].Op != "FROM" {
		return nil, fmt.Errorf("runner Dockerfile must start with FROM")
	}
	return out, nil
}

// BuildPlan is the executable form of a parsed runner Dockerfile: the base image
// to start from, the ordered shell commands to run inside the rootfs, and the
// ENV/WORKDIR metadata to carry onto the run: service (NOT written into the
// rootfs). It is what BuildRunnerRootfs walks.
type BuildPlan struct {
	BaseImage string
	Runs      []string          // shell commands, in order, each a `RUN` body
	Env       map[string]string // baked process env (ENV instructions)
	EnvOrder  []string          // ENV keys in declaration order (stable apply)
	Workdir   string            // last WORKDIR wins ("" if none)
}

// PlanFromInstructions folds parsed instructions into a BuildPlan. ENV/WORKDIR
// are collected as metadata; RUN bodies are collected in order; FROM sets the
// base. A second FROM (multi-stage) is rejected — the runner image is single
// stage by construction.
func PlanFromInstructions(ins []Instruction) (BuildPlan, error) {
	p := BuildPlan{Env: map[string]string{}}
	for _, in := range ins {
		switch in.Op {
		case "FROM":
			if p.BaseImage != "" {
				return BuildPlan{}, fmt.Errorf("multi-stage FROM is not supported by the runner builder")
			}
			p.BaseImage = in.Value
		case "RUN":
			p.Runs = append(p.Runs, in.Value)
		case "ENV":
			if _, seen := p.Env[in.Key]; !seen {
				p.EnvOrder = append(p.EnvOrder, in.Key)
			}
			p.Env[in.Key] = in.Env
		case "WORKDIR":
			p.Workdir = in.Value
		}
	}
	return p, nil
}

// RunnerBuildPlan is the convenience entry: render the recipe, parse it, and
// fold it into an executable plan in one call.
func RunnerBuildPlan(r Runner) (BuildPlan, error) {
	df, err := renderDockerfile(r)
	if err != nil {
		return BuildPlan{}, err
	}
	ins, err := ParseRunnerDockerfile(df)
	if err != nil {
		return BuildPlan{}, err
	}
	return PlanFromInstructions(ins)
}
