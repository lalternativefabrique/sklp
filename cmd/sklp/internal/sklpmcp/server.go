// Package sklpmcp serves sklp to an AI agent over the Model Context Protocol:
// the local spaces, dev stacks and pipelines of this machine, and, when a
// deploy context is named, the node behind it.
package sklpmcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lalternativefabrique/sklp/internal/config"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
)

type Options struct {
	// Context names the deploy context whose node the deploy_* tools pilot.
	// Empty serves the local tools only: the active context is never assumed.
	Context  string
	ReadOnly bool
	Version  string
	// Exe is the sklp binary the local tools run; empty means this process's.
	Exe string
	// Warn receives the best-effort notices of an apply; stdout belongs to
	// the protocol.
	Warn io.Writer
}

type node struct {
	context string
	server  string
	token   string
	pin     string
	warn    io.Writer

	mu      sync.Mutex
	clients map[string]*control.Client
}

func NewServer(opts Options) (*mcp.Server, error) {
	s, _, err := newServer(opts)
	return s, err
}

func newServer(opts Options) (*mcp.Server, *local, error) {
	if opts.Warn == nil {
		opts.Warn = io.Discard
	}
	exe := opts.Exe
	if exe == "" {
		self, err := os.Executable()
		if err != nil {
			return nil, nil, err
		}
		exe = self
	}
	loc := &local{exe: exe, jobs: &jobs{byID: map[string]*job{}}}

	var n *node
	if opts.Context != "" {
		var err error
		if n, err = openNode(opts.Context, opts.Warn); err != nil {
			return nil, nil, err
		}
	}

	instructions := "sklp: local dev stacks (dev_*), closed environments (space_*), pipelines such as ci (run_pipeline, job_*), " +
		"discovered with stacks. Long runs return a job id: poll job_status. " +
		"Before writing or editing any file under .sklp/, never from memory: sklp_guide for that contract, sklp_draft for a starting point, " +
		"sklp_schema for the exact fields, then contract_validate what you wrote and fix it until it passes."
	if n != nil {
		instructions += fmt.Sprintf(" deploy_* tools pilot the node of deploy context %q (%s): "+
			"a node runs spaces, each the services and jobs of one deploy.yaml stack.", n.context, n.server)
	} else {
		instructions += " No deploy context is served; restart with -context <name> to pilot a node."
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "sklp", Version: opts.Version}, &mcp.ServerOptions{Instructions: instructions})

	defs := loc.tools()
	defs = append(defs, define(&mcp.Tool{Name: "deploy_contexts", Description: "List the deploy contexts configured on this machine " +
		"(name, node URL, default space). The deploy_* tools target only the one this server was started with (bound=true)."}, read, listContexts(n)))
	if n != nil {
		defs = append(defs, n.tools()...)
	}
	secrets := knownSecrets(n)
	for _, t := range defs {
		if opts.ReadOnly && t.kind != read {
			continue
		}
		t.add(s, secrets)
	}
	loc.addGuidePrompts(s)
	return s, loc, nil
}

func Run(ctx context.Context, opts Options) error {
	s, loc, err := newServer(opts)
	if err != nil {
		return err
	}
	err = s.Run(ctx, &mcp.StdioTransport{})
	stopCtx, cancel := context.WithTimeout(context.Background(), stopGrace+5*time.Second)
	defer cancel()
	for _, j := range loc.jobs.all() {
		j.stop(stopCtx)
	}
	return err
}

func openNode(contextName string, warn io.Writer) (*node, error) {
	cfg, _, err := config.Load()
	if err != nil {
		return nil, err
	}
	name, c, err := cfg.ResolveContext(contextName)
	if err != nil {
		return nil, err
	}
	n := &node{
		context: name,
		server:  c.Server,
		token:   config.EffectiveContextToken(c),
		pin:     c.CAFingerprint,
		warn:    warn,
		clients: map[string]*control.Client{},
	}
	switch {
	case n.server == "":
		return nil, fmt.Errorf("context %q has no server (sklp deploy login --context %s --server ...)", name, name)
	case n.token == "":
		return nil, fmt.Errorf("context %q has no token (sklp deploy login)", name)
	case n.pin == "":
		return nil, errors.New("context " + name + " has no ca_fingerprint (sklp deploy login)")
	}
	return n, nil
}

// spaceClient returns the client scoped to space, "" being the node itself.
func (n *node) spaceClient(space string) (*control.Client, error) {
	if space != "" {
		if err := control.ValidSpaceName(space); err != nil {
			return nil, err
		}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if c, ok := n.clients[space]; ok {
		return c, nil
	}
	c, err := control.DialNet(n.server, n.token, control.WithCAFingerprint(n.pin))
	if err != nil {
		return nil, err
	}
	if space != "" {
		c = c.WithSpace(space)
	}
	n.clients[space] = c
	return c, nil
}
