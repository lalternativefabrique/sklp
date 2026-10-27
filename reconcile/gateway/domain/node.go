package domain

import (
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Node is a sklp host reachable over its control API. It fronts every app of a
// STACK: one machine serves the whole stack's spaces, so the node belongs to
// the stack rather than to any single project inside it.
//
// Identity is an address Identity is an address
// plus a bearer token and a pinned certificate fingerprint — no PKI and no
// resolvable name, which is what lets a node be a pod today and a VM tomorrow.
//
// The token is held encrypted and never leaves the repository in clear.
type Node struct {
	ID             string
	StackID        string
	Name           string
	ServerURL      string
	EncryptedToken []byte
	CAFingerprint  string
	LastAppliedAt  *time.Time
	LastError      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// NewNode validates and builds a node. encryptedToken is the already-encrypted
// bearer token: the domain never sees the clear value.
// ErrSpaceRequired: a node hosts several spaces, so every space-scoped call
// must name the one it means.
var ErrSpaceRequired = errors.New("space is required")

func NewNode(stackID, name, serverURL string, encryptedToken []byte, caFingerprint string) (*Node, error) {
	if strings.TrimSpace(stackID) == "" {
		return nil, errors.New("stack_id is required")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("name is required")
	}
	serverURL, err := normalizeServerURL(serverURL)
	if err != nil {
		return nil, err
	}
	if len(encryptedToken) == 0 {
		return nil, errors.New("token is required")
	}
	now := time.Now().UTC()
	return &Node{
		ID:             uuid.New().String(),
		StackID:        stackID,
		Name:           name,
		ServerURL:      serverURL,
		EncryptedToken: encryptedToken,
		CAFingerprint:  strings.TrimSpace(caFingerprint),
		CreatedAt:      now,
		UpdatedAt:      now,
	}, nil
}

// RecordApplied marks a successful convergence toward the node.
// Reconfigure moves a registered node to a new address and, when given, new
// credentials. An empty encryptedToken keeps the current one; the fingerprint
// is always replaced, since a node that moved usually re-issued its cert.
func (n *Node) Reconfigure(serverURL string, encryptedToken []byte, caFingerprint string) error {
	normalized, err := normalizeServerURL(serverURL)
	if err != nil {
		return err
	}
	n.ServerURL = normalized
	if len(encryptedToken) > 0 {
		n.EncryptedToken = encryptedToken
	}
	n.CAFingerprint = strings.TrimSpace(caFingerprint)
	n.LastError = ""
	n.UpdatedAt = time.Now().UTC()
	return nil
}

func (n *Node) RecordApplied(at time.Time) {
	utc := at.UTC()
	n.LastAppliedAt = &utc
	n.LastError = ""
	n.UpdatedAt = time.Now().UTC()
}

// RecordFailure stores why the last push did not converge, so the UI can show a
// node that is configured but unreachable rather than silently stale.
func (n *Node) RecordFailure(msg string) {
	n.LastError = msg
	n.UpdatedAt = time.Now().UTC()
}

// normalizeServerURL requires https: the control API applies manifests and
// carries a bearer token, so plain http would put both on the wire.
func normalizeServerURL(raw string) (string, error) {
	raw = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw), "/"))
	if raw == "" {
		return "", errors.New("server_url is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("server_url is not a valid URL")
	}
	if u.Scheme != "https" {
		return "", errors.New("server_url must use https")
	}
	if u.Host == "" {
		return "", errors.New("server_url must carry a host")
	}
	return raw, nil
}
