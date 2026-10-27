// Package infrastructure holds the deploy-config bounded context's Postgres
// repository and its node-side HTTP transport.
package nodeapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/lalternativefabrique/sklp/reconcile/deployconfig/domain"
)

// ControlApplier POSTs a Manifest to a node's control API. It mirrors
// gateway/infrastructure.ControlPusher's transport (bearer token + pinned
// self-signed cert) — the same authentication scheme every sklp control-API
// caller uses, confirmed by issue 206ec5bd's transport research: apply,
// state and spaces routes share one token, no per-route scoping.
type ControlApplier struct {
	timeout time.Duration
	// client, when set, replaces the per-node pinned client. Only tests set
	// it, so a test server's own certificate is accepted without pinning.
	client *http.Client
}

func NewControlApplier() *ControlApplier {
	return &ControlApplier{timeout: 60 * time.Second}
}

// applyRequest mirrors spacemanager/control.ApplyRequest's source form: the
// node resolves the raw deploy.yaml itself with the resolved ${VAR} values,
// the same path `sklp deploy` takes, so nothing declared in the file is lost
// in transit. Force is never set: an ownership conflict must surface.
type applyRequest struct {
	Source *applySource `json:"source"`
}

type applySource struct {
	YAML  string            `json:"yaml"`
	Vars  map[string]string `json:"vars,omitempty"`
	Stack string            `json:"stack,omitempty"`
}

// applyResponse mirrors spacemanager/control.ApplyResult.
type applyResponse struct {
	Result   string   `json:"result"`
	Changes  []string `json:"changes,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// Apply implements domain.Applier.
func (a *ControlApplier) Apply(ctx context.Context, target domain.NodeTarget, m *domain.Manifest) (domain.ApplyResult, error) {
	body, err := json.Marshal(applyRequest{Source: &applySource{YAML: string(m.Raw), Vars: m.Vars, Stack: target.Stack}})
	if err != nil {
		return domain.ApplyResult{}, fmt.Errorf("marshal apply request: %w", err)
	}

	if a.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.timeout)
		defer cancel()
	}

	url := target.ServerURL + applyPath(target.Space)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return domain.ApplyResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+target.Token)

	resp, err := a.httpClient(target).Do(req)
	if err != nil {
		return domain.ApplyResult{}, fmt.Errorf("reach node: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return domain.ApplyResult{}, fmt.Errorf("node refused the apply: status %d: %s", resp.StatusCode, bytes.TrimSpace(msg))
	}
	var out applyResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return domain.ApplyResult{}, fmt.Errorf("decode apply response: %w", err)
	}
	return domain.ApplyResult{Result: out.Result, Changes: out.Changes, Warnings: out.Warnings}, nil
}

// applyPath picks /v1/spaces/<space>/apply (multi-space, per
// apps/sklp/spacemanager/control/protocol.go's SpaceRoutePrefix) when space
// is set — the only case Apply's caller ever produces, since
// domain.Manifest.Space is never empty. The empty-space branch stays as a
// defensive fallback to the legacy single-space /v1/apply, never exercised
// by ParseManifest's own output.
func applyPath(space string) string {
	if space == "" {
		return "/v1/apply"
	}
	return "/v1/spaces/" + space + "/apply"
}

func (a *ControlApplier) httpClient(target domain.NodeTarget) *http.Client {
	if a.client != nil {
		return a.client
	}
	return clientFor(target.CAFingerprint)
}

func pinnedVerifier(want string) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		for _, raw := range rawCerts {
			sum := sha256.Sum256(raw)
			if hex.EncodeToString(sum[:]) == want {
				return nil
			}
		}
		return errors.New("node certificate does not match the pinned fingerprint")
	}
}

// normalizeFingerprint accepts the shapes an operator may paste: with a
// "sha256:" prefix, colon-separated, or upper-case. Duplicated from
// gateway/infrastructure.ControlPusher rather than shared: the two contexts
// deliberately do not import each other's infrastructure package (only
// gatewaydomain, at the interface level — see reconcile_deploy/handler.go).
func normalizeFingerprint(fp string) string {
	fp = strings.TrimSpace(fp)
	fp = strings.TrimPrefix(strings.ToLower(fp), "sha256:")
	return strings.ReplaceAll(fp, ":", "")
}
