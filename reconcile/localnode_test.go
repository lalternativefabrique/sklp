package reconcile

import (
	"context"
	"errors"
	"strings"
	"testing"

	deployapply "github.com/lalternativefabrique/sklp/reconcile/deployconfig/apply"
	"github.com/lalternativefabrique/sklp/reconcile/gitsource"
	imagesync "github.com/lalternativefabrique/sklp/reconcile/imageupdate/sync"
	"github.com/lalternativefabrique/sklp/reconcile/source"
)

func localNode() *LocalNode {
	return &LocalNode{
		Name:          "node-a",
		ControlURL:    "https://127.0.0.1:6464",
		Token:         "node-token",
		CAFingerprint: "sha256:deadbeef",
	}
}

func TestLocalNodeAnswersItsOwnName(t *testing.T) {
	got, err := localNode().Credential(context.Background(), "", "node-a")
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	if got.ServerURL != "https://127.0.0.1:6464" {
		t.Errorf("ServerURL = %q, want the loopback control API", got.ServerURL)
	}
	if string(got.SealedToken) != "node-token" {
		t.Errorf("token = %q, want the node's own", got.SealedToken)
	}
}

// The guard that matters: a repo declaring another node must not deploy here.
// Several repos feed one stack and any of them can name any node, so answering
// "yes, that's me" to every name would apply another machine's manifest onto
// this one.
func TestLocalNodeRefusesAnotherNodesName(t *testing.T) {
	_, err := localNode().Credential(context.Background(), "", "node-c")
	if !errors.Is(err, deployapply.ErrNodeNotFound) {
		t.Fatalf("err = %v, want ErrNodeNotFound", err)
	}
	if !strings.Contains(err.Error(), "node-a") {
		t.Errorf("err = %v, want it to name this node so the mismatch is readable", err)
	}
}

// A node cannot know the registry id core gave it — that lives in core's
// database. Resolving one to this node would apply a manifest that may be
// meant for another machine entirely, so it is refused with the fix in the
// message.
func TestLocalNodeRefusesALegacyNodeID(t *testing.T) {
	_, err := localNode().CredentialByID(context.Background(), "", "192940ec-2d7e-45b3-b07d-06cff65fd05b")
	if !errors.Is(err, deployapply.ErrNodeNotFound) {
		t.Fatalf("err = %v, want ErrNodeNotFound", err)
	}
	if !strings.Contains(err.Error(), "node: node-a") {
		t.Errorf("err = %v, want it to state the declaration that would work", err)
	}
}

func TestPlainTokenHandsBackWhatItWasGiven(t *testing.T) {
	got, err := PlainToken{}.Decrypt([]byte("node-token"))
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if got != "node-token" {
		t.Fatalf("got %q, want the token unchanged", got)
	}
}

// A sentinel is identity, not text. These three pairs each live in different
// packages under the same name and the same message, so a producer returning
// the wrong one compiles and silently never matches — which is how a repo with
// no image-watches.yaml came to report a failed pass.

func TestGitsourceReturnsTheContractSentinel(t *testing.T) {
	if !errors.Is(gitsource.ErrFileNotFound, source.ErrFileNotFound) {
		t.Fatal("gitsource declares its own ErrFileNotFound; a handler checking the contract's would never match it")
	}
}

// The watch directory wraps LocalNode, which refuses with deployapply's
// sentinel. sync checks its own, so the refusal has to be re-raised as that
// one on the way through.
func TestWatchDirectoryTranslatesTheRefusal(t *testing.T) {
	d := nodeWatchDirectory{localNode()}

	_, err := d.Credential(context.Background(), "", "some-other-node")
	if !errors.Is(err, imagesync.ErrNodeNotFound) {
		t.Fatalf("err = %v, want sync's ErrNodeNotFound", err)
	}

	_, err = d.CredentialByID(context.Background(), "", "192940ec-2d7e")
	if !errors.Is(err, imagesync.ErrNodeNotFound) {
		t.Fatalf("err = %v, want sync's ErrNodeNotFound", err)
	}
}

func TestWatchDirectoryPassesItsOwnNameThrough(t *testing.T) {
	got, err := nodeWatchDirectory{localNode()}.Credential(context.Background(), "", "node-a")
	if err != nil {
		t.Fatalf("Credential: %v", err)
	}
	if got.ServerURL != "https://127.0.0.1:6464" {
		t.Fatalf("ServerURL = %q, want the loopback control API", got.ServerURL)
	}
}
