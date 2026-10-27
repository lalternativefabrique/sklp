package sklet

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/lalternativefabrique/sklp/space"
	"github.com/lalternativefabrique/sklp/space/spaceapi"
	"github.com/lalternativefabrique/sklp/spacemanager/control"
)

// installInventory wires the node-scoped space inventory served at
// GET /v1/spaces. The registry is built once per supervisor process but reads
// the runc state on every call, so a space created or torn down after this
// point is still reported: the inventory answers for the MACHINE, not for the
// space this supervisor happens to own.
//
// A registry that cannot be built (unreadable cache root) leaves the seam unset
// rather than installing one that reports an empty node — the route then
// answers 501, which a client tells apart from "runs nothing".
func installInventory(errOut io.Writer) {
	reg, err := space.NewRegistry(control.ListSpaces)
	if err != nil {
		if errOut != nil {
			fmt.Fprintln(errOut, "sklet: warning: no space inventory served:", err)
		}
		return
	}
	control.SetInventory(control.InventoryFunc(func(ctx context.Context) (json.RawMessage, error) {
		infos, lerr := reg.List(ctx)
		if lerr != nil {
			return nil, lerr
		}
		// Marshal the slice, not a nil map: an empty node must encode as [] so a
		// client reads "no spaces" rather than a null it has to special-case.
		if infos == nil {
			infos = []spaceapi.SpaceInfo{}
		}
		return json.Marshal(infos)
	}))
}
