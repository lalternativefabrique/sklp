// Package imagebuild is a small, daemonless Dockerfile builder for the
// project's APPLICATION images (multi-stage, COPY, COPY --from, secrets) — the
// "world 2" that the runner-image builder (runnerimage) deliberately excludes.
//
// It is NOT a general Docker/BuildKit reimplementation. It supports exactly the
// instruction set real project Dockerfiles use (measured across acme/skalpai/
// bakery): FROM..AS, RUN, RUN --mount=type=secret, COPY, COPY --from=<stage>,
// ENV, WORKDIR, USER, EXPOSE, ENTRYPOINT, CMD, ARG. Anything else fails loudly.
//
// The novel part is that multi-stage + COPY --from is cheap here: every stage
// is just a rootfs DIRECTORY on disk that we own (built via runtime.PrepareImage
// + the same copy+RUN machinery as the runner image). COPY --from=build is then
// a plain directory copy between two rootfs we control — no snapshot graph, no
// daemon. BuildKit builds a cathedral of snapshots for this; we copy two paths.
package imagebuild

import (
	"fmt"
	"strings"
)

// Op is a parsed Dockerfile instruction kind.
type Op string

const (
	OpFrom       Op = "FROM"
	OpRun        Op = "RUN"
	OpCopy       Op = "COPY"
	OpEnv        Op = "ENV"
	OpArg        Op = "ARG"
	OpWorkdir    Op = "WORKDIR"
	OpUser       Op = "USER"
	OpExpose     Op = "EXPOSE"
	OpEntrypoint Op = "ENTRYPOINT"
	OpCmd        Op = "CMD"
)

// Instruction is one logical Dockerfile instruction (line continuations already
// joined). Fields are populated per Op; see each builder consumer.
type Instruction struct {
	Op   Op
	Args string // raw argument text after the op keyword (continuations joined)
	Line int    // 1-based source line of the instruction's first line
}

// Stage is one FROM block: its base image, optional name (FROM x AS name), and
// the ordered instructions that build on it. A COPY --from references a stage by
// name or by index.
type Stage struct {
	Name         string        // "" if the FROM had no `AS <name>`
	BaseImage    string        // the FROM argument (an image ref or a prior stage name)
	BaseStageRef string        // set when BaseImage names a PRIOR stage (FROM build)
	Index        int           // 0-based position in the file
	Instructions []Instruction // everything between this FROM and the next (RUN/COPY/ENV/…)
}

// Dockerfile is the parsed, multi-stage program: the ordered stages. The last
// stage is the build target.
type Dockerfile struct {
	// GlobalArgs are ARG declarations BEFORE the first FROM (Docker's build-arg
	// scope). They are usable in FROM lines and, inside a stage, only after an
	// in-stage `ARG NAME` redeclaration — standard Docker semantics.
	GlobalArgs []Instruction
	Stages     []Stage
}

// Target returns the final stage (the build output).
func (d *Dockerfile) Target() *Stage {
	if len(d.Stages) == 0 {
		return nil
	}
	return &d.Stages[len(d.Stages)-1]
}

// stageByName resolves a COPY --from=<ref> target: <ref> is a stage name or a
// numeric index. Returns the stage and true, or false if unresolved.
func (d *Dockerfile) stageByName(ref string) (*Stage, bool) {
	for i := range d.Stages {
		if d.Stages[i].Name == ref {
			return &d.Stages[i], true
		}
	}
	// Numeric index form (COPY --from=0).
	var idx int
	if _, err := fmt.Sscanf(ref, "%d", &idx); err == nil && idx >= 0 && idx < len(d.Stages) {
		return &d.Stages[idx], true
	}
	return nil, false
}

// supportedOps is the instruction set this builder implements. Anything else is
// rejected at parse time so an unsupported Dockerfile fails loudly rather than
// producing a silently-wrong image.
var supportedOps = map[Op]bool{
	OpFrom: true, OpRun: true, OpCopy: true, OpEnv: true, OpArg: true,
	OpWorkdir: true, OpUser: true, OpExpose: true, OpEntrypoint: true, OpCmd: true,
}

// Parse reads a Dockerfile into ordered Stages. It joins backslash line
// continuations, skips comments/blank lines, and groups instructions under
// their FROM. It rejects unsupported instructions (ADD, HEALTHCHECK, ONBUILD,
// SHELL, VOLUME, LABEL, STOPSIGNAL) and a file that does not start with FROM.
func Parse(dockerfile string) (*Dockerfile, error) {
	lines, starts := joinContinuations(dockerfile)

	df := &Dockerfile{}
	var cur *Stage
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		opTok, rest, _ := strings.Cut(trimmed, " ")
		op := Op(strings.ToUpper(opTok))
		rest = strings.TrimSpace(rest)
		srcLine := starts[i]

		if !supportedOps[op] {
			return nil, fmt.Errorf("line %d: unsupported instruction %q — imagebuild handles FROM/RUN/COPY/ENV/ARG/WORKDIR/USER/EXPOSE/ENTRYPOINT/CMD only", srcLine, opTok)
		}

		if op == OpFrom {
			st := parseFrom(rest, len(df.Stages))
			df.Stages = append(df.Stages, st)
			cur = &df.Stages[len(df.Stages)-1]
			continue
		}
		// ARG before the first FROM is a global build-arg declaration: it is
		// usable in FROM lines and becomes visible in a stage only after an
		// in-stage `ARG NAME` redeclaration (Docker semantics). Any OTHER
		// instruction before a FROM is an error.
		if cur == nil {
			if op == OpArg {
				df.GlobalArgs = append(df.GlobalArgs, Instruction{Op: op, Args: rest, Line: srcLine})
				continue
			}
			return nil, fmt.Errorf("line %d: %s before any FROM", srcLine, op)
		}
		cur.Instructions = append(cur.Instructions, Instruction{Op: op, Args: rest, Line: srcLine})
	}

	if len(df.Stages) == 0 {
		return nil, fmt.Errorf("Dockerfile must contain at least one FROM")
	}
	resolveStageBases(df)
	return df, nil
}

// parseFrom parses `FROM <image> [AS <name>]` into a Stage.
func parseFrom(rest string, index int) Stage {
	st := Stage{Index: index}
	fields := strings.Fields(rest)
	if len(fields) >= 3 && strings.EqualFold(fields[1], "AS") {
		st.BaseImage = fields[0]
		st.Name = fields[2]
	} else if len(fields) >= 1 {
		st.BaseImage = fields[0]
	}
	return st
}

// resolveStageBases marks a stage whose FROM names a PRIOR stage (FROM build)
// rather than an image ref, so the builder layers it on that stage's rootfs
// instead of pulling.
func resolveStageBases(df *Dockerfile) {
	names := map[string]bool{}
	for _, s := range df.Stages {
		if s.Name != "" {
			names[s.Name] = true
		}
	}
	for i := range df.Stages {
		if names[df.Stages[i].BaseImage] {
			df.Stages[i].BaseStageRef = df.Stages[i].BaseImage
		}
	}
}

// joinContinuations merges backslash-continued lines into single logical lines,
// returning the logical lines and, for each, the 1-based source line where it
// began (for error messages).
func joinContinuations(dockerfile string) (lines []string, starts []int) {
	raw := strings.Split(dockerfile, "\n")
	i := 0
	for i < len(raw) {
		start := i + 1
		cur := strings.TrimRight(raw[i], "\r")
		for strings.HasSuffix(strings.TrimRight(cur, " \t"), "\\") {
			cur = strings.TrimRight(strings.TrimRight(cur, " \t"), "\\")
			i++
			if i >= len(raw) {
				break
			}
			cur += " " + strings.TrimRight(raw[i], "\r")
		}
		lines = append(lines, cur)
		starts = append(starts, start)
		i++
	}
	return lines, starts
}
