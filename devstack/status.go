package devstack

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"text/tabwriter"

	"github.com/lalternativefabrique/sklp/pipeline"
)

// StackStatus is one dev stack a `sklp dev` run recorded on this machine.
type StackStatus struct {
	Repo          string `json:"repo"`
	Stack         string `json:"stack"`
	Running       bool   `json:"running"`
	SupervisorPID int    `json:"supervisor_pid"`
	Space         string `json:"space,omitempty"`
	Log           string `json:"log,omitempty"`
}

// Stacks lists every recorded dev stack of this user, whatever the repo.
func Stacks() ([]StackStatus, error) {
	paths, err := filepath.Glob(filepath.Join(stateDir(), "dev-*.json"))
	if err != nil {
		return nil, err
	}
	out := []StackStatus{}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var st runState
		if json.Unmarshal(data, &st) != nil || st.Repo == "" {
			continue
		}
		out = append(out, StackStatus{
			Repo: st.Repo, Stack: st.Stack, SupervisorPID: st.SupervisorPID, Space: st.EmbeddedSpace,
			Running: st.SupervisorPID > 0 && !pidDead(st.SupervisorPID),
			Log:     latestRunLog(st.Repo, st.Stack),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Stack < out[j].Stack
	})
	return out, nil
}

// latestRunLog is the newest run-log of the stack: the central dev log, or the
// detached child's log when no central one was opened.
func latestRunLog(repoAbs, stack string) string {
	logs, _ := filepath.Glob(filepath.Join(pipeline.CentralLogDir(repoAbs), devLogName(stack, "*")))
	if len(logs) > 0 {
		sort.Strings(logs)
		return logs[len(logs)-1]
	}
	if p := detachLogPath(repoAbs, stack); fileExists(p) {
		return p
	}
	return ""
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// RunLs is the `sklp dev ls [-json]` entrypoint.
func RunLs(args []string) int {
	relayToRunningVM(args)
	fs := flag.NewFlagSet("sklp dev ls", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	asJSON := fs.Bool("json", false, "emit JSON on stdout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	stacks, err := Stacks()
	if err != nil {
		fmt.Fprintf(os.Stderr, "sklp dev ls: %v\n", err)
		return 1
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(stacks); err != nil {
			return 1
		}
		return 0
	}
	if len(stacks) == 0 {
		fmt.Println("no dev stack recorded")
		return 0
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "STACK\tSTATUS\tPID\tREPO")
	for _, s := range stacks {
		status := "stopped"
		if s.Running {
			status = "running"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", stackLabel(s.Stack), status, s.SupervisorPID, s.Repo)
	}
	_ = tw.Flush()
	return 0
}

// RunLogs is the `sklp dev logs <stack> [-repo .] [-n 100]` entrypoint: the
// tail of the stack's latest run-log.
func RunLogs(args []string) int {
	relayToRunningVM(args)
	fs := flag.NewFlagSet("sklp dev logs", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	repo := fs.String("repo", ".", "repo root of the stack")
	lines := fs.Int("n", 100, "number of last lines to print")
	positionals, err := parsePositionals(fs, args)
	if err != nil {
		return 2
	}
	if len(positionals) != 1 {
		fmt.Fprintln(os.Stderr, "sklp dev logs: exactly one stack is required")
		return 2
	}
	repoAbs, err := filepath.Abs(*repo)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sklp dev logs: resolve -repo: %v\n", err)
		return 2
	}
	path := latestRunLog(repoAbs, positionals[0])
	if path == "" {
		fmt.Fprintf(os.Stderr, "sklp dev logs: no run-log for stack %q in %s\n", positionals[0], repoAbs)
		return 1
	}
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sklp dev logs: %v\n", err)
		return 1
	}
	defer f.Close()
	n := max(*lines, 1)
	ring := make([]string, 0, n)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		if len(ring) == n {
			ring = ring[1:]
		}
		ring = append(ring, sc.Text())
	}
	for _, l := range ring {
		fmt.Println(l)
	}
	return 0
}
