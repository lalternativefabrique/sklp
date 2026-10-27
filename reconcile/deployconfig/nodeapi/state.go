package nodeapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/lalternativefabrique/sklp/reconcile/deployconfig/domain"
)

// ControlStateFetcher reads a space's live state from a node's control API.
// Unlike gateway/infrastructure.StateClient (which GETs the unprefixed
// /v1/state — the space the node's supervisor happened to start on, not a
// selectable one), this always targets /v1/spaces/<space>/state: the same
// SpaceRoutePrefix scheme apps/sklp/spacemanager/control/protocol.go defines
// for RouteState on a multi-space node, and the one ControlApplier already
// uses for RouteApply — so a health check after a reconcile_deploy Apply
// asks about the SAME space the apply targeted, not whichever one the
// supervisor booted into first.
type ControlStateFetcher struct {
	timeout time.Duration
	client  *http.Client // test override, same convention as ControlApplier
}

func NewControlStateFetcher() *ControlStateFetcher {
	return &ControlStateFetcher{timeout: 15 * time.Second}
}

// stateResponse mirrors spacemanager/control.StateResponse's wire shape
// (apps/sklp/spacemanager/control/protocol.go) — that struct DOES carry
// explicit json tags (unlike Service/SpaceMeta — see control_applier.go's
// applySpec doc), so this mirror matches those tags rather than relying on
// Go's default field-name encoding.
type stateResponse struct {
	Space    string                `json:"space"`
	AllReady bool                  `json:"all_ready"`
	Services []serviceStatePayload `json:"services"`
}

type serviceStatePayload struct {
	Name      string                 `json:"name"`
	State     string                 `json:"state"`
	Instances []instanceStatePayload `json:"instances"`
}

type instanceStatePayload struct {
	ID    string `json:"id"`
	State string `json:"state"`
	PID   int    `json:"pid"`
}

// State implements domain.StateFetcher.
func (f *ControlStateFetcher) State(ctx context.Context, target domain.NodeTarget) (domain.NodeState, error) {
	if f.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, f.timeout)
		defer cancel()
	}

	url := target.ServerURL + statePath(target.Space)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return domain.NodeState{}, err
	}
	req.Header.Set("Authorization", "Bearer "+target.Token)

	resp, err := f.httpClient(target).Do(req)
	if err != nil {
		return domain.NodeState{}, fmt.Errorf("reach node: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return domain.NodeState{}, fmt.Errorf("node refused the state request: status %d: %s", resp.StatusCode, msg)
	}

	var raw stateResponse
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return domain.NodeState{}, fmt.Errorf("decode node state: %w", err)
	}
	return toNodeState(raw), nil
}

// statePath picks /v1/spaces/<space>/state — target.Space is always
// non-empty (domain.ParseManifest defaults it to "production"), same
// contract as control_applier.go's applyPath.
func statePath(space string) string {
	if space == "" {
		return "/v1/state"
	}
	return "/v1/spaces/" + space + "/state"
}

func toNodeState(raw stateResponse) domain.NodeState {
	services := make([]domain.ServiceState, 0, len(raw.Services))
	for _, s := range raw.Services {
		instances := make([]domain.InstanceState, 0, len(s.Instances))
		for _, i := range s.Instances {
			instances = append(instances, domain.InstanceState{ID: i.ID, State: i.State, PID: i.PID})
		}
		services = append(services, domain.ServiceState{Name: s.Name, State: s.State, Instances: instances})
	}
	return domain.NodeState{Space: raw.Space, AllReady: raw.AllReady, Services: services}
}

func (f *ControlStateFetcher) httpClient(target domain.NodeTarget) *http.Client {
	if f.client != nil {
		return f.client
	}
	return clientFor(target.CAFingerprint)
}
