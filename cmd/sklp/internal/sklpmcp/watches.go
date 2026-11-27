package sklpmcp

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lalternativefabrique/sklp/spacemanager/control"
)

const (
	maxWatches      = 100
	maxRollsInWatch = 5
)

func (n *node) watchTools() []toolDef {
	return []toolDef{
		define(&mcp.Tool{Name: "deploy_watches", Description: "List the image watches the node rolls by itself: for each, the space and service, " +
			"the image and its current tag, held/pinned tags, the last error and the latest rolls (newest first)."}, read, n.listWatches),
	}
}

type watchesInput struct {
	Space string `json:"space,omitempty" jsonschema:"only the watches of this space"`
	Repo  string `json:"repo,omitempty" jsonschema:"only the watches of this followed repo"`
}

type watchesOut struct {
	Node      string               `json:"node"`
	Watches   []control.WatchEntry `json:"watches"`
	Truncated bool                 `json:"truncated,omitempty"`
}

func (n *node) listWatches(ctx context.Context, in watchesInput, _ *redactor) (watchesOut, error) {
	c, err := n.spaceClient("")
	if err != nil {
		return watchesOut{}, err
	}
	res, err := c.Watches(ctx)
	if err != nil {
		return watchesOut{}, err
	}
	out := watchesOut{Node: res.Node, Watches: []control.WatchEntry{}}
	for _, w := range res.Watches {
		if (in.Space != "" && w.Space != in.Space) || (in.Repo != "" && w.Repo != in.Repo) {
			continue
		}
		if len(out.Watches) == maxWatches {
			out.Truncated = true
			break
		}
		w.Rolls = nonNil(head(w.Rolls, maxRollsInWatch))
		out.Watches = append(out.Watches, w)
	}
	return out, nil
}
