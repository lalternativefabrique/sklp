package control

import (
	"context"
	"encoding/json"
	"os"
	"sync"
)

// Inventory reports every space present on the NODE. It is deliberately NOT a
// method of Supervisor: a Supervisor owns exactly one space, while an inventory
// answers for the machine — including spaces no supervisor runs, and spaces
// belonging to other repos entirely.
//
// The result is opaque JSON (an encoded []spaceapi.SpaceInfo) so this seam, the
// DTO and the client stay compilable without the embedded_runtime tag; only the
// node-side implementation touches the runtime types.
type Inventory interface {
	SpacesJSON(ctx context.Context) (json.RawMessage, error)
}

// InventoryFunc adapts a plain function to Inventory.
type InventoryFunc func(ctx context.Context) (json.RawMessage, error)

// SpacesJSON implements Inventory.
func (f InventoryFunc) SpacesJSON(ctx context.Context) (json.RawMessage, error) {
	return f(ctx)
}

var (
	inventoryMu sync.RWMutex
	inventory   Inventory
)

// SetInventory installs the node-scoped inventory source served at
// RouteSpaces. It is a process-wide registration rather than a Serve/ServeNet
// parameter because a node answers for its machine once, whereas Serve is
// called per space: threading it through every call site would ask each
// supervisor to carry a value that is not its own. Left unset, RouteSpaces
// reports 501 instead of guessing an empty inventory — a node that cannot
// enumerate its spaces must not be indistinguishable from an empty one.
func SetInventory(inv Inventory) {
	inventoryMu.Lock()
	defer inventoryMu.Unlock()
	inventory = inv
}

func currentInventory() Inventory {
	inventoryMu.RLock()
	defer inventoryMu.RUnlock()
	return inventory
}

// NodeName identifies the machine serving the control API. The hostname is the
// only identifier a node has that an operator already recognises; an unreadable
// hostname yields "" rather than a fabricated id, leaving the caller to label
// the row by the address it dialled.
func NodeName() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	return name
}
