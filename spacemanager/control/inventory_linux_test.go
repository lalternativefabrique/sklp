//go:build linux

package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

// setInventoryForTest installs inv for the duration of the test and restores
// the previous one, so the process-wide seam cannot leak across tests.
func setInventoryForTest(t *testing.T, inv Inventory) {
	t.Helper()
	prev := currentInventory()
	SetInventory(inv)
	t.Cleanup(func() { SetInventory(prev) })
}

// A node with an inventory reports EVERY space on the machine, not just the one
// the serving supervisor owns — the whole point of the node-scoped route.
func TestSpacesReportsWholeNode(t *testing.T) {
	setInventoryForTest(t, InventoryFunc(func(ctx context.Context) (json.RawMessage, error) {
		return json.RawMessage(`[{"name":"production","status":"running"},{"name":"other-repo","status":"stopped"}]`), nil
	}))

	c, cancel := serveFake(t, "production", &fakeSupervisor{})
	defer cancel()

	res, err := c.Spaces(context.Background())
	if err != nil {
		t.Fatalf("spaces: %v", err)
	}
	var got []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(res.Spaces, &got); err != nil {
		t.Fatalf("decode spaces: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 spaces, got %d: %s", len(got), res.Spaces)
	}
	// The space this supervisor does NOT own must still be listed.
	if got[1].Name != "other-repo" || got[1].Status != "stopped" {
		t.Fatalf("second space not reported verbatim: %+v", got[1])
	}
	if res.Node == "" {
		t.Fatal("node name is empty; a multi-node view cannot label the row")
	}
}

// A node that serves no inventory must be distinguishable from a node that runs
// nothing: the client reports ErrNoInventory, never an empty list.
func TestSpacesWithoutInventoryIsNotAnEmptyNode(t *testing.T) {
	setInventoryForTest(t, nil)

	c, cancel := serveFake(t, "production", &fakeSupervisor{})
	defer cancel()

	res, err := c.Spaces(context.Background())
	if !errors.Is(err, ErrNoInventory) {
		t.Fatalf("want ErrNoInventory, got err=%v res=%+v", err, res)
	}
}

// An inventory that fails to read the node surfaces as an error, so a partial
// view is never mistaken for the truth.
func TestSpacesInventoryErrorSurfaces(t *testing.T) {
	setInventoryForTest(t, InventoryFunc(func(ctx context.Context) (json.RawMessage, error) {
		return nil, errors.New("cache root unreadable")
	}))

	c, cancel := serveFake(t, "production", &fakeSupervisor{})
	defer cancel()

	if _, err := c.Spaces(context.Background()); err == nil {
		t.Fatal("want an error when the inventory cannot be read, got nil")
	}
}

// The node inventory names every space and repo on the machine, so the NETWORK
// listener must gate it exactly like every other route: anonymous callers get
// 401, never the list.
func TestSpacesRequiresTokenOverNetwork(t *testing.T) {
	setInventoryForTest(t, InventoryFunc(func(ctx context.Context) (json.RawMessage, error) {
		return json.RawMessage(`[{"name":"production"}]`), nil
	}))
	srv, fp := newTestNetServer(t, &fakeSupervisor{})

	for _, tc := range []struct {
		name   string
		header string
		want   int
	}{
		{"no token", "", http.StatusUnauthorized},
		{"wrong token", "Bearer nope", http.StatusUnauthorized},
		{"right token", "Bearer " + testToken, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, srv.URL+RouteSpaces, nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			resp, err := pinnedHTTPClient(fp).Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}
