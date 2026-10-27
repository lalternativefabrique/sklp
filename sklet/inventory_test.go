//go:build linux

package sklet

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/lalternativefabrique/sklp/spacemanager/control"
)

// installInventory must serve THIS machine's spaces through the real registry:
// the seam is what makes a node enumerable, and a silent failure would leave
// every node looking empty rather than broken.
func TestInstallInventoryServesRealNode(t *testing.T) {
	ln, _, err := control.Listen("inventory-probe")
	if err != nil {
		t.Skipf("no runtime dir for a control socket: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go control.Serve(ctx, "inventory-probe", &stubSupervisor{}, ln, cancel)

	installInventory(os.Stderr)

	c, err := control.Dial("inventory-probe")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	res, err := c.Spaces(ctx)
	if err != nil {
		t.Fatalf("spaces: %v", err)
	}
	var got []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(res.Spaces, &got); err != nil {
		t.Fatalf("decode %s: %v", res.Spaces, err)
	}
	if res.Node == "" {
		t.Error("node name is empty; a multi-node view cannot label this node's rows")
	}
	t.Logf("node=%q spaces=%d", res.Node, len(got))
}
