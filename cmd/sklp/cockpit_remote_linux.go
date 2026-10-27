//go:build linux

// cockpit_remote.go feeds the cockpit the spaces running on the OTHER machines
// this workstation is configured to reach. The nodes are the deploy contexts
// already in the config (server + token + ca-fingerprint): a context IS a node,
// so a VM shows up in the cockpit as soon as it is deployable, with nothing
// extra to declare.
package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/lalternativefabrique/sklp/cockpit"
	"github.com/lalternativefabrique/sklp/internal/config"
	"github.com/lalternativefabrique/sklp/space/spaceapi"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
)

// nodeReadTimeout bounds ONE node's inventory read. The cockpit redraws on a
// timer, so a machine that is powered off or behind a dropped route must fail
// fast: without this the whole view would stall on the slowest node.
const nodeReadTimeout = 3 * time.Second

// listRemoteNodes reads every configured node's inventory concurrently and
// returns one entry per node, in stable name order. Failures are returned as
// entries carrying their error, never dropped: a node that vanished from the
// list would read as a machine running nothing.
func listRemoteNodes(ctx context.Context) []cockpit.RemoteNode {
	cfg, _, err := config.Load()
	if err != nil {
		return []cockpit.RemoteNode{{Name: "config", Err: err}}
	}
	names := make([]string, 0, len(cfg.Contexts))
	for name := range cfg.Contexts {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]cockpit.RemoteNode, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			out[i] = readNode(ctx, name, cfg.Contexts[name])
		}(i, name)
	}
	wg.Wait()
	return out
}

// listContexts names every configured deploy context WITHOUT dialling any of
// them: the picker draws at startup, before the operator has chosen a node, so
// it must cost nothing. Local is added by the cockpit itself.
func listContexts() []cockpit.ContextEntry {
	cfg, _, err := config.Load()
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(cfg.Contexts))
	for name := range cfg.Contexts {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]cockpit.ContextEntry, 0, len(names))
	for _, name := range names {
		out = append(out, cockpit.ContextEntry{Name: name, Target: cfg.Contexts[name].Server})
	}
	return out
}

// readNode fetches one node's inventory over the network control API.
func readNode(ctx context.Context, name string, c config.Context) cockpit.RemoteNode {
	node := cockpit.RemoteNode{Name: name, Target: c.Server}
	if c.Server == "" {
		node.Err = errors.New("no server configured for this context")
		return node
	}
	token := config.EffectiveContextToken(c)
	if token == "" {
		node.Err = errors.New("no token for this context (see `sklp deploy login`)")
		return node
	}
	client, err := control.DialNet(c.Server, token, control.WithCAFingerprint(c.CAFingerprint))
	if err != nil {
		node.Err = err
		return node
	}
	ctx, cancel := context.WithTimeout(ctx, nodeReadTimeout)
	defer cancel()
	res, err := client.Spaces(ctx)
	if err != nil {
		node.Err = nodeReadErr(err)
		return node
	}
	infos, err := spaceapi.DecodeSpaceInfos(res.Spaces)
	if err != nil {
		node.Err = fmt.Errorf("decode inventory: %w", err)
		return node
	}
	node.Spaces = make([]cockpit.SpaceSummary, 0, len(infos))
	for _, info := range infos {
		runc := make([]cockpit.ContainerSummary, 0, len(info.Containers))
		for _, ct := range info.Containers {
			runc = append(runc, cockpit.ContainerSummary{ID: ct.ID, Status: ct.Status})
		}
		sum := cockpit.SpaceSummary{
			Name:       info.Name,
			Status:     info.Status,
			Runc:       runc,
			Volumes:    info.Volumes,
			Supervised: info.Supervised,
			Repo:       info.Repo,
		}
		node.Spaces = append(node.Spaces, sum)
	}
	applyNodeState(ctx, c, token, node.Spaces)
	return node
}

// applyNodeState upgrades the container-level inventory with the supervised
// truth: real service names — what the control API keys logs and restarts on,
// as opposed to the `<svc>-<instance>` ids runc reports — plus readiness.
//
// A node dispatches each of its spaces under SpaceRoutePrefix, so every
// supervised space is read on its own routes, concurrently: they share one
// deadline and a serial pass would spend it on the first few. A space that
// cannot answer keeps the coarser inventory view, which stays valid.
func applyNodeState(ctx context.Context, c config.Context, token string, spaces []cockpit.SpaceSummary) {
	var wg sync.WaitGroup
	for i := range spaces {
		if !spaces[i].Supervised {
			continue
		}
		wg.Add(1)
		go func(sum *cockpit.SpaceSummary) {
			defer wg.Done()
			// One client per space: WithSpace scopes the client it is called on,
			// so sharing a single one across these goroutines would race on it.
			client, err := control.DialNet(c.Server, token, control.WithCAFingerprint(c.CAFingerprint))
			if err != nil {
				return
			}
			st, err := client.WithSpace(sum.Name).State(ctx)
			if err != nil || st.Space != sum.Name {
				return
			}
			svcs := make([]cockpit.ServiceSummary, 0, len(st.Services))
			for _, svc := range st.Services {
				svcs = append(svcs, cockpit.ServiceSummary{
					Name:      svc.Name,
					State:     svc.State,
					Instances: len(svc.Instances),
				})
			}
			sum.Services = svcs
			sum.AllReady = st.AllReady
		}(&spaces[i])
	}
	wg.Wait()
}

// nodeReadErr turns a transport failure into something that describes the NODE.
// The client's own errors are phrased for a single space ("no supervisor
// running for space") because that is what the socket transport addresses; over
// the network the caller asked a machine for its inventory, and a message about
// a space it never named only misleads.
func nodeReadErr(err error) error {
	switch {
	case errors.Is(err, control.ErrNoInventory):
		return errors.New("node runs a sklet older than /v1/spaces — upgrade it")
	case errors.Is(err, control.ErrNoSupervisor):
		return errors.New("nothing is listening on the control API — is sklet running there?")
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("no answer within %s — unreachable or firewalled", nodeReadTimeout)
	default:
		return err
	}
}

// logClient returns the control client to stream a space's logs from: the local
// Unix socket for a local space, or the node's network control API.
//
// A node dispatches every space it hosts under SpaceRoutePrefix, so the space
// the context happens to name is not a limit on what can be streamed — the
// client is scoped to the space actually being read, which is what puts the
// request on that space's routes.
func logClient(node, space string) (*control.Client, error) {
	if node == "" {
		return control.Dial(space)
	}
	cfg, _, err := config.Load()
	if err != nil {
		return nil, err
	}
	c, ok := cfg.Contexts[node]
	if !ok {
		return nil, fmt.Errorf("node %q is no longer configured", node)
	}
	token := config.EffectiveContextToken(c)
	if token == "" {
		return nil, fmt.Errorf("no token for node %q (see `sklp deploy login`)", node)
	}
	client, err := control.DialNet(c.Server, token, control.WithCAFingerprint(c.CAFingerprint))
	if err != nil {
		return nil, err
	}
	return client.WithSpace(space), nil
}
