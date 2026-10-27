package main

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strings"
	"syscall"

	"github.com/lalternativefabrique/sklp/space"
	"github.com/lalternativefabrique/sklp/space/spacenet"
)

// reap.go holds the runtime-reaping helpers `sklp clean` builds its plan from:
// which cgroups back a live deployment (and must survive), which space ids a
// normal teardown can still reach, and how each pass is rendered.

// liveDeploymentCgroups returns the set of cgroup basenames that back a space
// with at least one RUNNING container — the deployments a clean must never
// touch. A cgroup id is "sklp-deploy-<svc>-<i>"; a running container's id
// matches its cgroup basename exactly, so protecting by id is precise.
func liveDeploymentCgroups() map[string]bool {
	protected := map[string]bool{}
	for _, s := range listSpaces() {
		infos, err := space.ListDeployContainers(s.Name)
		if err != nil {
			continue
		}
		for _, c := range infos {
			if strings.EqualFold(c.Status, "running") {
				protected[c.ID] = true
			}
		}
	}
	return protected
}

func killPid(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) }

func printProcPlan(out io.Writer, r spacenet.OrphanProcReapResult) {
	if len(r.SlirpKilled) == 0 && len(r.WorkerKilled) == 0 {
		fmt.Fprintln(out, "  ORPHAN PROCS: none")
		return
	}
	fmt.Fprintf(out, "  ORPHAN PROCS: %d slirp, %d worker(s)\n", len(r.SlirpKilled), len(r.WorkerKilled))
}

func printCgroupPlan(out io.Writer, r spacenet.CgroupReapResult) {
	if len(r.EmptyRemoved) == 0 && len(r.Killed) == 0 {
		fmt.Fprintln(out, "  RESIDUAL CGROUPS: none")
		return
	}
	sort.Strings(r.EmptyRemoved)
	fmt.Fprintf(out, "  RESIDUAL CGROUPS: %s", strings.Join(dedup(r.EmptyRemoved), ", "))
	if len(r.Killed) > 0 {
		fmt.Fprintf(out, " (kill %d pause proc)", len(r.Killed))
	}
	fmt.Fprintln(out)
}

// printUnownedCgroupPlan reports cgroups whose space no longer exists. Their
// processes are named, not just counted: they are real workloads (a nats-server,
// a postgres), so the user must see what is about to die.
func printUnownedCgroupPlan(out io.Writer, names []string) {
	if len(names) == 0 {
		fmt.Fprintln(out, "  ORPHANED SPACE CGROUPS: none")
		return
	}
	sort.Strings(names)
	fmt.Fprintf(out, "  ORPHANED SPACE CGROUPS (space gone, unreachable by `space down`): %s\n",
		strings.Join(dedup(names), ", "))
}

// notIn returns the entries of in that are absent from exclude.
func notIn(in, exclude []string) []string {
	drop := map[string]bool{}
	for _, e := range exclude {
		drop[e] = true
	}
	var out []string
	for _, s := range in {
		if !drop[s] {
			out = append(out, s)
		}
	}
	return out
}

// knownSpacePrefixes is the id prefix of every space still present on disk — the
// cgroups a normal teardown can still reach, hence must not be force-reaped.
func knownSpacePrefixes() []string {
	var out []string
	for _, s := range listSpaces() {
		out = append(out, space.DeployIDPrefix(s.Name))
	}
	return out
}

// confirmDestructive prompts default-NO (unlike init's confirm which defaults
// yes): the run deletes deployment state or data, so a bare Enter must abort.
func confirmDestructive(out io.Writer, in io.Reader) bool {
	fmt.Fprint(out, "\nProceed? [y/N] ")
	line, _ := bufio.NewReader(in).ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}

func dedup(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
