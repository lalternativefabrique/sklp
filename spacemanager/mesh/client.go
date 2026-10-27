package mesh

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Peer mirrors meshauthority's peer shape (apps/meshauthority/internal/mesh/http.go).
type Peer struct {
	ID        string `json:"id"`
	PubKey    string `json:"pubkey"`
	SignKey   string `json:"sign_key,omitempty"`
	PrivateIP string `json:"private_ip"`
	Endpoint  string `json:"endpoint,omitempty"`
	// Spaces lists the Space names this peer's node last reported running
	// locally (ADR-008) — how the cross-host relay resolves "which node
	// runs Space X".
	Spaces []string `json:"spaces,omitempty"`
	// Services lists the (space, service, port) triples this peer's
	// Network Controller last reported serving locally — how cross-node
	// DNS resolution finds which node's mesh IP hosts a given service.
	Services []ServiceRef `json:"services,omitempty"`
	// LastSeenAt is when this peer last registered or heartbeated with
	// meshauthority — the freshness signal consumers use to stop resolving
	// a dead node's services (the authority itself never evicts: it is a
	// passive directory, judging liveness is the reader's call).
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
}

// ServiceRef mirrors meshauthority's ServiceRef (one service a node's
// Network Controller serves locally).
type ServiceRef struct {
	Space   string `json:"space"`
	Service string `json:"service"`
	Port    int    `json:"port"`
}

type registerResponse struct {
	Peer  Peer   `json:"peer"`
	Peers []Peer `json:"peers"`
}

// Client talks to a meshauthority instance. The bearer token says the caller
// belongs to this mesh; signKey says WHICH node it is. The token alone used to
// be enough to re-register any node's ID and take over the traffic routed to
// its services, so every write is signed.
type Client struct {
	baseURL string
	token   string
	signKey ed25519.PrivateKey
	http    *http.Client
}

func NewClient(baseURL, token string, signKey ed25519.PrivateKey) *Client {
	return NewPinnedClient(baseURL, token, signKey, "")
}

// NewPinnedClient is NewClient with meshauthority's certificate pinned by
// SHA-256 fingerprint (hex, as `openssl x509 -fingerprint -sha256` prints it,
// colons optional).
//
// meshauthority serves a self-signed certificate: there is no CA to validate
// against, and skipping verification outright would leave the bearer token
// readable by anything on the path. Pinning gives the node exactly one
// certificate it will talk to. A node refuses to start without one
// (CheckAuthority); an empty fingerprint here only serves tests.
func NewPinnedClient(baseURL, token string, signKey ed25519.PrivateKey, certFingerprint string) *Client {
	httpClient := &http.Client{Timeout: 10 * time.Second}
	if fp := normaliseFingerprint(certFingerprint); fp != "" {
		httpClient.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{
				// The certificate is self-signed, so chain verification cannot
				// succeed; the fingerprint below is what authenticates it.
				InsecureSkipVerify: true,
				VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
					return verifyPinnedLeaf(rawCerts, fp)
				},
			},
		}
	}
	return &Client{
		baseURL: baseURL,
		token:   token,
		signKey: signKey,
		http:    httpClient,
	}
}

func normaliseFingerprint(fp string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(fp), ":", ""))
}

// signingPayload must render byte-for-byte what meshauthority's own
// SigningPayload builds — the two live in separate Go modules, so the contract
// is duplicated rather than imported, and a drift breaks every write with a
// 401. TestSigningPayloadMatchesAuthority pins it.
func signingPayload(method, path string, timestamp int64, body []byte) []byte {
	sum := sha256.Sum256(body)
	return []byte(strings.Join([]string{
		method,
		path,
		strconv.FormatInt(timestamp, 10),
		hex.EncodeToString(sum[:]),
	}, "\n"))
}

// Register submits this node's public key + dialable endpoint + current
// local Space and service lists, and returns its own assigned mesh IP
// alongside the full current peer list. spaces and services are each a
// whole-list-replace per call (ADR-008 and its cross-node extension): call
// it again whenever either changes to keep the registry current.
func (c *Client) Register(ctx context.Context, nodeID, pubkey, endpoint string, spaces []string, services []ServiceRef) (self Peer, peers []Peer, err error) {
	body, err := json.Marshal(map[string]any{"id": nodeID, "pubkey": pubkey, "endpoint": endpoint, "spaces": spaces, "services": services})
	if err != nil {
		return Peer{}, nil, fmt.Errorf("marshal register request: %w", err)
	}

	var out registerResponse
	if err := c.do(ctx, http.MethodPost, "/v1/peers/register", body, &out); err != nil {
		return Peer{}, nil, err
	}
	return out.Peer, out.Peers, nil
}

// Heartbeat keeps this node marked alive in meshauthority's registry.
func (c *Client) Heartbeat(ctx context.Context, nodeID string) error {
	return c.do(ctx, http.MethodPost, "/v1/peers/"+nodeID+"/heartbeat", nil, nil)
}

// ListPeers fetches the current full peer list.
func (c *Client) ListPeers(ctx context.Context) ([]Peer, error) {
	var out []Peer
	if err := c.do(ctx, http.MethodGet, "/v1/peers", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// DeletePeer asks meshauthority, signed as this node, to forget a peer that
// has been silent long enough to be gone.
func (c *Client) DeletePeer(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/peers/"+url.PathEscape(id), nil, nil)
}

func (c *Client) do(ctx context.Context, method, path string, body []byte, out any) error {
	var reader *bytes.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	} else {
		reader = bytes.NewReader(nil)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.signKey != nil {
		ts := time.Now().Unix()
		req.Header.Set("X-Mesh-Sign-Key", hex.EncodeToString(c.signKey.Public().(ed25519.PublicKey)))
		req.Header.Set("X-Mesh-Timestamp", strconv.FormatInt(ts, 10))
		req.Header.Set("X-Mesh-Signature", hex.EncodeToString(ed25519.Sign(c.signKey, signingPayload(method, req.URL.Path, ts, body))))
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("meshauthority request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		var reason struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&reason)
		if reason.Error != "" {
			return fmt.Errorf("meshauthority %s %s: status %d: %s", method, path, resp.StatusCode, reason.Error)
		}
		return fmt.Errorf("meshauthority %s %s: status %d", method, path, resp.StatusCode)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode meshauthority response: %w", err)
	}
	return nil
}

// verifyPinnedLeaf matches the pin against the leaf only: the handshake proves
// possession of the leaf's key alone, so any other certificate in the chain is
// attacker-supplied.
func verifyPinnedLeaf(rawCerts [][]byte, fp string) error {
	if len(rawCerts) == 0 {
		return fmt.Errorf("meshauthority presented no certificate")
	}
	if sum := sha256.Sum256(rawCerts[0]); hex.EncodeToString(sum[:]) != fp {
		return fmt.Errorf("meshauthority certificate does not match the pinned fingerprint")
	}
	return nil
}
