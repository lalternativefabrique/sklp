package nodeapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/lalternativefabrique/sklp/reconcile/imageupdate/domain"
)

const imageCallTimeout = 60 * time.Second

// ControlRoller rolls a watched service onto a tag through the control API of
// the node that runs it, and reads that node's rollout records back.
type ControlRoller struct{}

func NewControlRoller() *ControlRoller { return &ControlRoller{} }

type imageRequest struct {
	Service string `json:"service"`
	Image   string `json:"image"`
}

type imageStateResponse struct {
	Services []struct {
		Name     string `json:"name"`
		Image    string `json:"image"`
		Rollouts []struct {
			ID         string    `json:"id"`
			FromImage  string    `json:"from_image"`
			ToImage    string    `json:"to_image"`
			Result     string    `json:"result"`
			Reason     string    `json:"reason"`
			ExitCode   int       `json:"exit_code"`
			LogTail    []string  `json:"log_tail"`
			StartedAt  time.Time `json:"started_at"`
			FinishedAt time.Time `json:"finished_at"`
		} `json:"rollouts"`
	} `json:"services"`
}

func (r *ControlRoller) RunningImage(ctx context.Context, w *domain.Watch) (string, error) {
	state, err := r.state(ctx, w)
	if err != nil {
		return "", err
	}
	for _, svc := range state.Services {
		if svc.Name == w.Service {
			return svc.Image, nil
		}
	}
	return "", fmt.Errorf("service %q not found in space %q on %s", w.Service, w.NodeSpace, w.NodeServer)
}

func (r *ControlRoller) Rollouts(ctx context.Context, w *domain.Watch) ([]domain.NodeRollout, error) {
	state, err := r.state(ctx, w)
	if err != nil {
		return nil, err
	}
	var out []domain.NodeRollout
	for _, svc := range state.Services {
		for _, ro := range svc.Rollouts {
			out = append(out, domain.NodeRollout{
				ID: ro.ID, Service: svc.Name, FromImage: ro.FromImage, ToImage: ro.ToImage,
				Result: ro.Result, Reason: ro.Reason, ExitCode: ro.ExitCode, LogTail: ro.LogTail,
				StartedAt: ro.StartedAt, FinishedAt: ro.FinishedAt,
			})
		}
	}
	return out, nil
}

// Roll posts the new image to the node and maps its answer onto a roll result.
// A node reporting `unchanged` already runs the image, which is what was asked.
func (r *ControlRoller) Roll(ctx context.Context, w *domain.Watch, tag string) (string, error) {
	body, err := json.Marshal(imageRequest{Service: w.Service, Image: w.ImageRef(tag)})
	if err != nil {
		return "", err
	}
	resp, err := r.do(ctx, w, http.MethodPost, spacedRoute(w.NodeSpace, "/v1/image"), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// The node's 400 names the service or image it rejected. An auth
		// failure's body adds nothing, and this error lands in the UI.
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return "", fmt.Errorf("node %s rejected the token: status %d", w.NodeServer, resp.StatusCode)
		}
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", fmt.Errorf("node %s: status %d: %s", w.NodeServer, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var res applyResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", fmt.Errorf("decode node response: %w", err)
	}
	switch res.Result {
	case "unchanged":
		return domain.RollResultUnchanged, nil
	case "rejected":
		reason := strings.Join(append(res.Changes, res.Warnings...), "; ")
		if reason == "" {
			reason = "the node refused the spec"
		}
		return "", fmt.Errorf("node %s rejected the roll: %s", w.NodeServer, reason)
	}
	return domain.RollResultApplied, nil
}

func (r *ControlRoller) state(ctx context.Context, w *domain.Watch) (imageStateResponse, error) {
	resp, err := r.do(ctx, w, http.MethodGet, spacedRoute(w.NodeSpace, "/v1/state"), nil)
	if err != nil {
		return imageStateResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return imageStateResponse{}, fmt.Errorf("node %s: status %d: %s", w.NodeServer, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var state imageStateResponse
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		return imageStateResponse{}, fmt.Errorf("decode node state: %w", err)
	}
	return state, nil
}

func (r *ControlRoller) do(ctx context.Context, w *domain.Watch, method, route string, body io.Reader) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, imageCallTimeout)
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(w.NodeServer, "/")+route, body)
	if err != nil {
		cancel()
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+w.NodeToken)
	resp, err := clientFor(w.NodeCAFingerprint).Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("reach node %s: %w", w.NodeServer, err)
	}
	resp.Body = cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// spacedRoute addresses a space-scoped route: a multi-space node 404s the bare
// form unless it runs exactly one space.
func spacedRoute(space, route string) string {
	if space == "" {
		return route
	}
	return "/v1/spaces/" + space + strings.TrimPrefix(route, "/v1")
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}
