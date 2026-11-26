// Package mcpnode serves one sklet node over the Model Context Protocol, with
// the access a `sklp deploy -context <name>` command has and nothing more.
package mcpnode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lalternativefabrique/sklp/internal/config"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
)

type Options struct {
	Context  string
	ReadOnly bool
	Version  string
	// Warn receives the best-effort notices of an apply (project secrets
	// skipped); stdout belongs to the protocol.
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
	if opts.Context == "" {
		return nil, errors.New("-context is required: sklp mcp never targets the active context implicitly")
	}
	cfg, _, err := config.Load()
	if err != nil {
		return nil, err
	}
	name, c, err := cfg.ResolveContext(opts.Context)
	if err != nil {
		return nil, err
	}
	n := &node{
		context: name,
		server:  c.Server,
		token:   config.EffectiveContextToken(c),
		pin:     c.CAFingerprint,
		warn:    opts.Warn,
		clients: map[string]*control.Client{},
	}
	switch {
	case n.server == "":
		return nil, fmt.Errorf("context %q has no server (sklp deploy login --context %s --server ...)", name, name)
	case n.token == "":
		return nil, fmt.Errorf("context %q has no token (sklp deploy login)", name)
	case n.pin == "":
		return nil, fmt.Errorf("context %q has no ca_fingerprint (sklp deploy login)", name)
	}
	if n.warn == nil {
		n.warn = io.Discard
	}

	s := mcp.NewServer(&mcp.Implementation{Name: "sklp", Version: opts.Version}, &mcp.ServerOptions{
		Instructions: fmt.Sprintf("Pilots the sklet node of deploy context %q (%s). "+
			"A node runs spaces; a space runs the services and jobs of one deploy.yaml stack. "+
			"Read with list_spaces, get_space, get_manifest, service_logs and list_datastores; "+
			"apply_stack deploys a stack from a local repo.", name, n.server),
	})
	for _, t := range n.tools() {
		if opts.ReadOnly && t.destructive {
			continue
		}
		t.add(s)
	}
	return s, nil
}

func Run(ctx context.Context, opts Options) error {
	s, err := NewServer(opts)
	if err != nil {
		return err
	}
	return s.Run(ctx, &mcp.StdioTransport{})
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
