package sklpmcp

import (
	"context"
	"errors"
	"net/url"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lalternativefabrique/sklp/spacemanager/control"
)

const maxFollows = 100

func (n *node) followTools() []toolDef {
	return []toolDef{
		define(&mcp.Tool{Name: "deploy_follows", Description: "List the public repos the node follows: for each, the node applies the repo's " +
			".sklp/deploy-root.yaml and rolls its image watches by itself."}, read, n.listFollows),
		define(&mcp.Tool{Name: "deploy_follow", Description: "Have the node follow a public git repo (sklp deploy follow <repo>): within a minute it " +
			"deploys the repo's deploy-root stacks and keeps them in step with the branch and the image watches. " +
			"Refused when the repo or the name is already followed."}, write, n.follow),
		define(&mcp.Tool{Name: "deploy_unfollow", Description: "Stop following a repo (see deploy_follows). Its spaces keep running until deleted " +
			"with deploy_delete, but no longer track the repo."}, destructive, n.unfollow),
	}
}

type followOut struct {
	Name    string    `json:"name"`
	Repo    string    `json:"repo"`
	Branch  string    `json:"branch"`
	AddedAt time.Time `json:"added_at"`
}

type followsOut struct {
	Node      string      `json:"node"`
	Follows   []followOut `json:"follows"`
	Truncated bool        `json:"truncated,omitempty"`
}

func toFollowOut(f control.FollowEntry) followOut {
	return followOut{Name: f.Name, Repo: f.Repo, Branch: f.Branch, AddedAt: f.AddedAt}
}

func (n *node) listFollows(ctx context.Context, _ noInput, _ *redactor) (followsOut, error) {
	c, err := n.spaceClient("")
	if err != nil {
		return followsOut{}, err
	}
	res, err := c.Follows(ctx)
	if err != nil {
		return followsOut{}, err
	}
	out := followsOut{Node: res.Node, Follows: []followOut{}, Truncated: len(res.Follows) > maxFollows}
	for _, f := range head(res.Follows, maxFollows) {
		out.Follows = append(out.Follows, toFollowOut(f))
	}
	return out, nil
}

type followInput struct {
	Repo   string `json:"repo" jsonschema:"https URL of a public git repo with a .sklp/deploy-root.yaml"`
	Branch string `json:"branch,omitempty" jsonschema:"branch to follow (default main)"`
	Name   string `json:"name,omitempty" jsonschema:"name the node keeps the repo under (default: the repo's last path element)"`
}

func (n *node) follow(ctx context.Context, in followInput, _ *redactor) (followOut, error) {
	if in.Repo == "" {
		return followOut{}, errors.New("repo is required")
	}
	if u, err := url.Parse(in.Repo); err == nil && u.User != nil {
		return followOut{}, errors.New("the repo URL carries credentials: a node follows public repos only, give the URL without them")
	}
	c, err := n.spaceClient("")
	if err != nil {
		return followOut{}, err
	}
	f, err := c.Follow(ctx, control.FollowRequest{Repo: in.Repo, Branch: in.Branch, Name: in.Name})
	if err != nil {
		return followOut{}, err
	}
	return toFollowOut(f), nil
}

type unfollowInput struct {
	Name string `json:"name" jsonschema:"name of the followed repo (see deploy_follows)"`
}

type unfollowOut struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

func (n *node) unfollow(ctx context.Context, in unfollowInput, _ *redactor) (unfollowOut, error) {
	if in.Name == "" {
		return unfollowOut{}, errors.New("name is required (see deploy_follows)")
	}
	c, err := n.spaceClient("")
	if err != nil {
		return unfollowOut{}, err
	}
	if err := c.Unfollow(ctx, in.Name); err != nil {
		return unfollowOut{}, err
	}
	return unfollowOut{Name: in.Name, Status: "no longer followed; its spaces keep running until deleted"}, nil
}
