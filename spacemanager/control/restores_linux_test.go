//go:build linux

package control

import (
	"context"
	"encoding/json"
	"testing"
)

func TestRestoresReachesTheNodeRoute(t *testing.T) {
	prev := currentRestores()
	SetRestores(RestoreSourceFunc(func(context.Context) (json.RawMessage, error) {
		return json.RawMessage(`[{"name":"shop-production/postgres","history":[]}]`), nil
	}))
	t.Cleanup(func() { SetRestores(prev) })

	c, cancel := serveFake(t, "production", &fakeSupervisor{})
	defer cancel()
	res, err := c.Restores(context.Background())
	if err != nil {
		t.Fatalf("restores: %v", err)
	}
	var got []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(res.Targets, &got); err != nil || len(got) != 1 || got[0].Name != "shop-production/postgres" {
		t.Fatalf("targets = %s (%v)", res.Targets, err)
	}
}
