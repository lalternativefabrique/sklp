package devstack

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/lalternativefabrique/sklp/internal/layout"
	"github.com/lalternativefabrique/sklp/pipeline"
)

const forwardUsage = "usage: sklp dev forward <stack> <service>[:<port>] [<host-port>]"

type forwardTarget struct {
	Service  string
	Port     int
	HostPort int
}

func parseForwardArgs(args []string) (stack string, t forwardTarget, err error) {
	if len(args) < 2 || len(args) > 3 {
		return "", t, errors.New(forwardUsage)
	}
	stack = args[0]
	svc, portStr, hasPort := strings.Cut(args[1], ":")
	if svc == "" {
		return "", t, errors.New(forwardUsage)
	}
	t.Service = svc
	if hasPort {
		if t.Port, err = parsePort(portStr); err != nil {
			return "", t, fmt.Errorf("service port %q: %w", portStr, err)
		}
	}
	if len(args) == 3 {
		if t.HostPort, err = parsePort(args[2]); err != nil {
			return "", t, fmt.Errorf("host port %q: %w", args[2], err)
		}
	}
	return stack, t, nil
}

func parsePort(s string) (int, error) {
	p, err := strconv.Atoi(s)
	if err != nil || p < 1 || p > 65535 {
		return 0, errors.New("must be a number between 1 and 65535")
	}
	return p, nil
}

func resolveForwardPort(stack *Stack, t forwardTarget) (int, error) {
	for _, ns := range stack.Services {
		if ns.Name != t.Service {
			continue
		}
		if t.Port > 0 {
			return t.Port, nil
		}
		var httpField string
		if ns.Def.Ready != nil {
			httpField = ns.Def.Ready.HTTP
		}
		if p, ok := pipeline.PortFromReadyHTTP(ns.Def.Port, httpField); ok {
			return p, nil
		}
		return 0, fmt.Errorf("service %q declares no port: add `port:` to it or pass %s:<port>", t.Service, t.Service)
	}
	names := make([]string, 0, len(stack.Services))
	for _, ns := range stack.Services {
		names = append(names, ns.Name)
	}
	return 0, fmt.Errorf("unknown service %q (services: %s)", t.Service, strings.Join(names, ", "))
}

// RunForward implements `sklp dev forward`.
func RunForward(args []string) int {
	fs := flag.NewFlagSet("sklp dev forward", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	repo := fs.String("repo", ".", "repo root whose dev stack to reach")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, forwardUsage)
		fs.PrintDefaults()
	}
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return 2
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}

	stackName, target, err := parseForwardArgs(positional)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sklp dev forward: %v\n", err)
		return 2
	}
	repoAbs, err := filepath.Abs(*repo)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sklp dev forward: resolve -repo: %v\n", err)
		return 2
	}
	taskPath, err := layout.New(repoAbs).Resolve(stackName, "dev")
	if err != nil {
		fmt.Fprintf(os.Stderr, "sklp dev forward: %v\n", err)
		return 2
	}
	stack, err := LoadOpts(taskPath, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sklp dev forward: %v\n", err)
		return 1
	}
	target.Port, err = resolveForwardPort(stack, target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sklp dev forward: %v\n", err)
		return 2
	}

	if code, relayed := relayForward(repoAbs, stackName, target); relayed {
		return code
	}

	st, err := readState(repoAbs, stackName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sklp dev forward: %v\n", err)
		return 1
	}
	if st == nil || st.SupervisorPID <= 0 || pidDead(st.SupervisorPID) {
		fmt.Fprintf(os.Stderr, "sklp dev forward: stack %q is not running here (start it with `sklp dev %s`)\n", stackName, stackName)
		return 1
	}
	if !st.Embedded {
		fmt.Fprintln(os.Stderr, "sklp dev forward: needs the embedded runtime (Linux)")
		return 1
	}
	spaceName := st.EmbeddedSpace
	if spaceName == "" {
		spaceName = devSpaceName(repoAbs, stackName)
	}
	return forwardToSpace(spaceName, st.SupervisorPID, target)
}
