package reconcile

import (
	"context"
	"fmt"

	deployapply "github.com/lalternativefabrique/sklp/reconcile/deployconfig/apply"
)

// localnode.go answers "where is this node and how do I reach it" when the
// answer is "here".
//
// Core resolves that against a registry: a deploy-root names a node, core looks
// up its address, opens the sealed bearer token, and dials across the internet.
// Every part of that exists because core is somewhere else. On the node, the
// supervisor is on loopback and there is no credential to open — the reconciler
// already runs as the user that owns it.

// LocalNode implements deployapply.NodeDirectory for the node itself.
//
// It answers with the loopback control API whatever name a deploy-root gives,
// with one guard: the name must be this node's own. A repo that names another
// node is not this node's work, and applying it here would deploy someone
// else's manifest onto this machine.
type LocalNode struct {
	// Name is what deploy-root.yaml must say to target this node.
	Name string
	// ControlURL is the supervisor's control API, normally on loopback.
	ControlURL string
	// Token authenticates to it. The control API requires a bearer even from
	// localhost, so this is read from the node's own credentials rather than
	// left empty.
	Token string
	// CAFingerprint pins the supervisor's self-signed certificate.
	CAFingerprint string
}

// Credential implements deployapply.NodeDirectory.
func (n *LocalNode) Credential(_ context.Context, _, nodeName string) (deployapply.NodeAddress, error) {
	if nodeName != n.Name {
		// Not an error the reconcile should abort on — the handler logs it and
		// skips that deployment — but it must never silently apply.
		return deployapply.NodeAddress{}, fmt.Errorf("%w: %q is not this node (%q)",
			deployapply.ErrNodeNotFound, nodeName, n.Name)
	}
	return n.address(), nil
}

// CredentialByID answers the superseded node_id form. A node cannot know
// which registry id it was given — that lives in core's database — so a
// deploy-root still naming one has to be migrated to a name before this node
// can reconcile it. Saying so beats resolving it to this node and applying a
// manifest meant for another.
func (n *LocalNode) CredentialByID(_ context.Context, _, nodeID string) (deployapply.NodeAddress, error) {
	return deployapply.NodeAddress{}, fmt.Errorf(
		"%w: node_id %q cannot be resolved on a node; declare `node: %s` instead",
		deployapply.ErrNodeNotFound, nodeID, n.Name)
}

func (n *LocalNode) address() deployapply.NodeAddress {
	return deployapply.NodeAddress{
		ServerURL:     n.ControlURL,
		CAFingerprint: n.CAFingerprint,
		SealedToken:   []byte(n.Token),
	}
}

// PlainToken implements deployapply.NodeResolver by handing back what
// LocalNode put in SealedToken.
//
// Core seals a node's token because it holds credentials for machines it does
// not run on. A node reading its own token from its own config has nothing to
// protect it from — the process already has the file — so the "decrypt" step
// is the identity function, said out loud rather than left as a puzzle.
type PlainToken struct{}

func (PlainToken) Decrypt(blob []byte) (string, error) { return string(blob), nil }
