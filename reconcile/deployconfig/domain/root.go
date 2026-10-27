package domain

import (
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// ErrNotARoot marks a file at RootPath that is not a deploy-root.
var ErrNotARoot = errors.New("not a deploy-root file")

// RootPath is where a repo declares which spaces it deploys and where each
// one's manifest lives. It is the only source of destination: a repo without
// it reconciles nothing.
const RootPath = ".sklp/deploy-root.yaml"

// Root is the deploy-root.yaml: one repo, several deployments. It is the
// pointer, not the payload — like an ArgoCD Application, it names a
// destination and a path, and the manifest at that path describes the
// services. This exists because core reconciles one space per manifest, so a
// repo owning several (an app plus the shared infrastructure it fronts) had
// no way to declare them all.
// RootKind is the `kind:` a deploy-root.yaml must declare. It exists so a
// file fetched from that path can be told apart from any other YAML: without
// it, a deploy.yaml served by a mis-scoped fetch would parse as a root with
// zero deployments and stop the reconciliation of a repo that has no root at
// all.
const RootKind = "deploy-root"

type Root struct {
	Kind string `yaml:"kind"`

	// Node is the default target for every deployment listed, named the way
	// the operator named it when registering the host — the same name the VM
	// carries. A name survives the address changing, which an IP written into
	// every repo would not, and it stays readable in a diff. An entry may
	// override it.
	Node string `yaml:"node"`
	// NodeID is the superseded way to name that same node: an opaque id only
	// the Skalpai database could resolve, which forced every reconcile to
	// consult the node registry before it could reach anything. Still read so
	// the roots already deployed keep reconciling; Server wins when both are
	// given.
	NodeID      string           `yaml:"node_id"`
	Deployments []RootDeployment `yaml:"deployments"`
}

// RootDeployment is one space this repo deploys, and the manifest that
// describes it.
type RootDeployment struct {
	Space  string `yaml:"space"`
	Path   string `yaml:"path"`
	Node   string `yaml:"node,omitempty"`
	NodeID string `yaml:"node_id,omitempty"`
}

// ParseRoot decodes a deploy-root.yaml and validates it. Every entry must
// name a space and a path: a partial entry is rejected outright rather than
// skipped, since a typo that silently drops a production space is worse than
// a reconciliation that refuses to run.
func ParseRoot(raw []byte) (*Root, error) {
	var r Root
	if err := yaml.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("%w: parse deploy-root: %v", ErrValidation, err)
	}
	if strings.TrimSpace(r.Kind) != RootKind {
		return nil, fmt.Errorf("%w: not a %s file (kind: %q)", ErrNotARoot, RootKind, r.Kind)
	}
	if len(r.Deployments) == 0 {
		return nil, fmt.Errorf("%w: deploy-root declares no deployments", ErrValidation)
	}
	r.Node = strings.TrimSpace(r.Node)
	r.NodeID = strings.TrimSpace(r.NodeID)
	seen := map[string]bool{}
	for i := range r.Deployments {
		d := &r.Deployments[i]
		d.Space = strings.TrimSpace(d.Space)
		d.Path = strings.TrimSpace(d.Path)
		d.Node = strings.TrimSpace(d.Node)
		d.NodeID = strings.TrimSpace(d.NodeID)
		if d.Space == "" {
			return nil, fmt.Errorf("%w: deploy-root entry %d declares no space", ErrValidation, i+1)
		}
		if d.Path == "" {
			return nil, fmt.Errorf("%w: deploy-root entry %q declares no path", ErrValidation, d.Space)
		}
		// An entry naming a node does not inherit the root's node_id, and
		// vice versa: inheriting the other half of a pair the entry already
		// answered is how a deployment silently targets the wrong node.
		if d.Node == "" && d.NodeID == "" {
			d.Node, d.NodeID = r.Node, r.NodeID
		}
		if d.Node != "" {
			d.NodeID = ""
		}
		if d.Node == "" && d.NodeID == "" {
			return nil, fmt.Errorf("%w: deploy-root entry %q names no node, and the root declares no default", ErrValidation, d.Space)
		}
		if seen[d.Space] {
			return nil, fmt.Errorf("%w: deploy-root declares space %q twice", ErrValidation, d.Space)
		}
		seen[d.Space] = true
	}
	return &r, nil
}
