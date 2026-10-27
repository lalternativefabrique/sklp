package control

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/lalternativefabrique/sklp/spacemanager"
)

// ErrNoSupervisor is returned (wrapped) by Dial/calls when the space socket
// is absent or refuses the connection — i.e. no supervisor is running for the
// space. The CLI maps this to an honest "space not deployed" message rather
// than a raw dial error.
var ErrNoSupervisor = errors.New("no supervisor running for space")

// Client talks the control API to a supervisor over EITHER a space's Unix
// socket (local, no auth) or an HTTPS network endpoint (remote, Bearer token +
// TLS). The two transports differ only in baseURL + how a raw connection is
// dialed for the exec hijack; every request method is shared.
//
//   - Local (Dial):   baseURL "http://sklp", c.http dials the UDS, dialRaw the UDS.
//   - Network (DialNet): baseURL "https://host:port", c.http is a TLS client with a
//     token-injecting RoundTripper, dialRaw a TLS conn to the same host.
type Client struct {
	space  string
	socket string // set for the local UDS transport; "" for network
	// baseURL is the scheme://host prefix every request is built on. The path
	// (RouteX) is appended. For the UDS transport the host is a dummy ("sklp")
	// since the transport dials the socket regardless.
	baseURL string
	http    *http.Client
	// dialRaw opens a raw stream to the supervisor for the exec hijack path
	// (HTTP/1.1 upgrade): a UDS conn locally, a TLS conn over the network.
	dialRaw func(ctx context.Context) (net.Conn, error)
	// rawHost is the Host header for the raw exec request line ("sklp" for the
	// UDS, "host:port" for the network).
	rawHost string
	// rawAuth is an extra request-line header line for the raw exec path
	// ("Authorization: Bearer <token>\r\n" over the network, "" locally).
	rawAuth string
	// routePrefix, when non-empty, is prepended to every space-scoped route
	// (RouteApply, RouteState, ...) so a multi-space network node can dispatch
	// the request to the right supervisor — see SpaceRoutePrefix. Empty for the
	// local UDS transport: the space is already identified by which socket file
	// the client dialed, so no path prefix is needed. RouteSpaces (node-scoped,
	// not space-scoped) never gets this prefix — see route().
	routePrefix string
}

// route resolves the wire path for a space-scoped constant (RouteApply,
// RouteState, RouteLogs, ...): the bare constant locally (the UDS socket
// already identifies the space), or SpaceRoutePrefix(space)+<constant minus
// its leading "/v1"> on a multi-space network client — e.g. RouteApply
// "/v1/apply" becomes "/v1/spaces/production/apply", matching what
// newMultiMux's dispatcher strips back off before forwarding to the space's
// own (unprefixed) mux. RouteSpaces is node-scoped and is returned bare
// whatever the client's scope: prefixing it would ask one space for the
// node's whole inventory.
func (c *Client) route(constant string) string {
	if c.routePrefix == "" || constant == RouteSpaces {
		return constant
	}
	return c.routePrefix + strings.TrimPrefix(constant, "/v1")
}

// Dial resolves the space's socket path and returns a Client. It does not
// connect yet — the first call surfaces ErrNoSupervisor if the socket is
// absent/refusing.
func Dial(space string) (*Client, error) {
	socket := SocketPath(space)
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	return &Client{
		space:   space,
		socket:  socket,
		baseURL: "http://sklp",
		// No global timeout: Logs(follow) is a long-lived stream cancelled via
		// the request context. Per-call deadlines are set on the request ctx.
		http: &http.Client{Transport: tr},
		dialRaw: func(ctx context.Context) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
		rawHost: "sklp",
	}, nil
}

// Health pings the supervisor.
func (c *Client) Health(ctx context.Context) (HealthResponse, error) {
	var out HealthResponse
	if err := c.getJSON(ctx, c.route(RouteHealth), &out); err != nil {
		return HealthResponse{}, err
	}
	return out, nil
}

// State returns the supervisor's whole-space view.
func (c *Client) State(ctx context.Context) (StateResponse, error) {
	var out StateResponse
	if err := c.getJSON(ctx, c.route(RouteState), &out); err != nil {
		return StateResponse{}, err
	}
	return out, nil
}

func (c *Client) Restores(ctx context.Context) (RestoresResponse, error) {
	var out RestoresResponse
	if err := c.getJSON(ctx, RouteRestores, &out); err != nil {
		return RestoresResponse{}, err
	}
	return out, nil
}

func (c *Client) DataStores(ctx context.Context) (DataStoresResponse, error) {
	var out DataStoresResponse
	if err := c.getJSON(ctx, c.route(RouteDataStores), &out); err != nil {
		return DataStoresResponse{}, err
	}
	return out, nil
}

// ErrNoInventory is returned by Spaces when the node answers but serves no
// inventory (RouteSpaces replies 501) — an older node, or one built without the
// runtime. It is distinct from an empty list on purpose: "this node cannot tell
// me" and "this node runs nothing" must not render the same.
var ErrNoInventory = errors.New("node serves no space inventory")

// Spaces lists every space on the NODE this client is connected to, whatever
// space the connection itself is scoped to. It is the node-scoped read a
// multi-node operator view is built on.
func (c *Client) Spaces(ctx context.Context) (SpacesResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+RouteSpaces, nil)
	if err != nil {
		return SpacesResponse{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return SpacesResponse{}, c.dialErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotImplemented {
		return SpacesResponse{}, ErrNoInventory
	}
	if resp.StatusCode != http.StatusOK {
		return SpacesResponse{}, fmt.Errorf("control: GET %s: status %d", RouteSpaces, resp.StatusCode)
	}
	var out SpacesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return SpacesResponse{}, err
	}
	return out, nil
}

// GatewayControlStatus reads the node's gateway singleton state (node-scoped,
// bare path like Spaces). A 503 means the node has no gateway opened.
func (c *Client) GatewayControlStatus(ctx context.Context) (GatewayControlStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var out GatewayControlStatus
	if err := c.nodeJSON(ctx, http.MethodGet, RouteGateway, &out); err != nil {
		return GatewayControlStatus{}, err
	}
	return out, nil
}

// GatewayControlUp asks the node to (re-)ensure its gateway singleton and
// returns the resulting status. Idempotent on a live gateway.
func (c *Client) GatewayControlUp(ctx context.Context) (GatewayControlStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	var out GatewayControlStatus
	if err := c.nodeJSON(ctx, http.MethodPost, RouteGatewayUp, &out); err != nil {
		return GatewayControlStatus{}, err
	}
	return out, nil
}

// GatewayCerts reads the certificate every routed host is served. The node
// dials each host, so the timeout covers a whole sweep, not one handshake.
func (c *Client) GatewayCerts(ctx context.Context) (GatewayCertsResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var out GatewayCertsResponse
	if err := c.nodeJSON(ctx, http.MethodGet, RouteGatewayCerts, &out); err != nil {
		return GatewayCertsResponse{}, err
	}
	return out, nil
}

// MeshPeers lists the mesh peers the node's meshauthority knows.
func (c *Client) MeshPeers(ctx context.Context) ([]MeshPeer, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var out MeshPeersResponse
	if err := c.nodeJSON(ctx, http.MethodGet, RouteMeshPeers, &out); err != nil {
		return nil, err
	}
	return out.Peers, nil
}

// DeleteMeshPeer asks the node to forget a mesh peer by id.
func (c *Client) DeleteMeshPeer(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return c.nodeJSON(ctx, http.MethodDelete, RouteMeshPeers+"/"+url.PathEscape(id), nil)
}

// RotateToken asks the node for a fresh Bearer token. The token this client
// dialled with is dead once the call returns, so the caller must persist the
// new one before anything else.
func (c *Client) RotateToken(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var out TokenRotateResponse
	if err := c.nodeJSON(ctx, http.MethodPost, RouteTokenRotate, &out); err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", fmt.Errorf("control: POST %s: empty token", RouteTokenRotate)
	}
	return out.Token, nil
}

// nodeJSON performs a node-scoped call (no space prefix) and decodes the JSON
// answer into out.
func (c *Client) nodeJSON(ctx context.Context, method, route string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+route, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return c.dialErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("control: %s %s: status %d: %s", method, route, resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Apply posts the desired manifest (opaque spec JSON built by MarshalApplySpec)
// to a live supervisor, which reconciles toward it and returns what changed. The
// CLI uses it for a re-apply: a 2nd `sklp deploy` of a running space talks to
// the live supervisor instead of starting a competing one.
func (c *Client) Apply(ctx context.Context, specJSON []byte) (ApplyResult, error) {
	return c.applyWithForce(ctx, specJSON, false)
}

// ApplyForce is Apply with the cross-stack ownership guard's escape hatch:
// the server adopts a space already owned by a different stack instead of
// refusing (see handleApply's guard, spacemanager/control/apply_linux.go).
func (c *Client) ApplyForce(ctx context.Context, specJSON []byte) (ApplyResult, error) {
	return c.applyWithForce(ctx, specJSON, true)
}

func (c *Client) applyWithForce(ctx context.Context, specJSON []byte, force bool) (ApplyResult, error) {
	return c.apply(ctx, ApplyRequest{Spec: json.RawMessage(specJSON), Force: force})
}

// ApplySource asks the node to resolve and apply a raw deploy.yaml itself.
func (c *Client) ApplySource(ctx context.Context, src ApplySource, force bool) (ApplyResult, error) {
	return c.apply(ctx, ApplyRequest{Source: &src, Force: force})
}

func (c *Client) apply(ctx context.Context, req ApplyRequest) (ApplyResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	body, err := json.Marshal(req)
	if err != nil {
		return ApplyResult{}, err
	}
	u := c.route(RouteApply)
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+u, bytes.NewReader(body))
	if err != nil {
		return ApplyResult{}, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(hreq)
	if err != nil {
		return ApplyResult{}, c.dialErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return ApplyResult{}, fmt.Errorf("control: POST %s: status %d: %s", u, resp.StatusCode, bytes.TrimSpace(msg))
	}
	var out ApplyResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ApplyResult{}, err
	}
	return out, nil
}

// SetGatewayRoutes replaces the node's whole gateway route set. Like Apply it
// is a convergence, not an append: routes absent from the argument are dropped
// from the live front door, and passing none clears them all. Changed reports
// whether the set actually moved, so a caller reconciling on a timer can tell a
// real convergence from a no-op.
func (c *Client) SetGatewayRoutes(ctx context.Context, routes []spacemanager.ResolvedRoute) (GatewayRoutesResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	body, err := json.Marshal(GatewayRoutesRequest{Routes: routes})
	if err != nil {
		return GatewayRoutesResponse{}, err
	}
	u := c.route(RouteGatewayRoutes)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.baseURL+u, bytes.NewReader(body))
	if err != nil {
		return GatewayRoutesResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return GatewayRoutesResponse{}, c.dialErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return GatewayRoutesResponse{}, fmt.Errorf("control: PUT %s: status %d: %s", u, resp.StatusCode, bytes.TrimSpace(msg))
	}
	var out GatewayRoutesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return GatewayRoutesResponse{}, err
	}
	return out, nil
}

// Down asks the supervisor to tear its space down from the inside. The server
// acks 202 (DownResponse{Stopping:true}) BEFORE it blocks on the teardown, so
// this call returns promptly; the caller then confirms the stop by watching the
// socket disappear. ErrNoSupervisor (no socket) means nothing to stop.
func (c *Client) Down(ctx context.Context) error {
	return c.down(ctx, false, false)
}

// DownAndPurge stops the space AND removes its persisted manifest and cache,
// so it does not return at the next boot — what `sklp deploy delete` means.
// Down alone stops it and leaves it startable.
func (c *Client) DownAndPurge(ctx context.Context) error {
	return c.down(ctx, true, false)
}

// DownAndDelete is DownAndPurge plus the named volumes: nothing of the space
// survives, its data included. This is the only teardown that leaves no ghost
// behind when the volumes live under the space dir.
func (c *Client) DownAndDelete(ctx context.Context) error {
	return c.down(ctx, true, true)
}

func (c *Client) down(ctx context.Context, purge, volumes bool) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	u := c.route(RouteDown)
	body, merr := json.Marshal(DownRequest{Purge: purge, Volumes: volumes})
	if merr != nil {
		return merr
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+u, bytes.NewReader(body))
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return c.dialErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("control: POST %s: status %d: %s", u, resp.StatusCode, bytes.TrimSpace(msg))
	}
	var out DownResponse
	if derr := json.NewDecoder(resp.Body).Decode(&out); derr != nil {
		return fmt.Errorf("control: POST %s: decode ack: %w", u, derr)
	}
	if !out.Stopping {
		return fmt.Errorf("control: POST %s: supervisor did not acknowledge stop", u)
	}
	return nil
}

// WaitGone polls until the space socket stops answering (the supervisor has
// finished its teardown and closed/removed it) or ctx expires. It is the
// confirmation step after Down: the 202 ack only promises the stop STARTED.
func (c *Client) WaitGone(ctx context.Context) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if !socketAnswers(c.socket) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// socketAnswers reports whether a supervisor still accepts on the socket. A
// refused/absent socket means the teardown finished. Tag-free (mirrors the
// gated socketAlive in server.go so the client compiles without the tag).
func socketAnswers(socket string) bool {
	conn, err := net.DialTimeout("unix", socket, 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// Logs streams a service's captured stdout/stderr to w. follow=false dumps the
// buffered tail then returns; follow=true streams until ctx is cancelled (the
// server stops its follower when the request context is done — no leaked
// goroutine). A 404 from the server (unknown service) is returned as an error.
func (c *Client) Logs(ctx context.Context, svc string, follow bool, w io.Writer) error {
	u := c.route(RouteLogs) + url.PathEscape(svc)
	if follow {
		u += "?follow=1"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+u, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// A cancelled follow is a clean stop, not a failure.
		if ctx.Err() != nil {
			return nil
		}
		return c.dialErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return c.unknownServiceErr(svc)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("control: GET %s: status %d", u, resp.StatusCode)
	}
	_, err = io.Copy(w, resp.Body)
	if err != nil && ctx.Err() != nil {
		return nil // client cancelled the follow; expected
	}
	return err
}

// unknownServiceErr phrases a 404 from a service lookup. The network transport
// carries no space name (the listener is scoped server-side), so naming an empty
// space would blame a scope the caller never chose — there the error names the
// service alone.
func (c *Client) unknownServiceErr(svc string) error {
	if c.space == "" {
		return fmt.Errorf("no service %q on this node's control API", svc)
	}
	return fmt.Errorf("service %q is not part of space %q", svc, c.space)
}

// RunJob re-runs a oneshot job that reached a terminal state (POST
// /v1/run/<job>). Unlike Restart it does not bounce a live process: the node
// re-launches the job from the spec it already holds. A 409 means a run is
// already in flight and is returned as an error, not swallowed as success.
func (c *Client) RunJob(ctx context.Context, job string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	u := c.route(RouteRun) + url.PathEscape(job)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+u, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return c.dialErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return c.unknownServiceErr(job)
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("control: POST %s: status %d: %s", u, resp.StatusCode, bytes.TrimSpace(msg))
	}
	return nil
}

// Restart bounces every live instance of a service (POST /v1/restart/<svc>). A
// 404 (unknown service) is returned as an error; the supervisor relaunches the
// instances itself, so this returns as soon as the bounce is acknowledged.
func (c *Client) Restart(ctx context.Context, svc string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	u := c.route(RouteRestart) + url.PathEscape(svc)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+u, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return c.dialErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return c.unknownServiceErr(svc)
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("control: POST %s: status %d: %s", u, resp.StatusCode, bytes.TrimSpace(msg))
	}
	return nil
}

// SetImage rolls one service onto a new image (POST /v1/image), reusing the
// manifest the node persisted. The caller needs neither the deploy.yaml nor the
// internal spec shape — only a service name and a tagged reference.
func (c *Client) SetImage(ctx context.Context, svc, image string) (ApplyResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	body, err := json.Marshal(ImageRequest{Service: svc, Image: image})
	if err != nil {
		return ApplyResult{}, err
	}
	u := c.route(RouteImage)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+u, bytes.NewReader(body))
	if err != nil {
		return ApplyResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return ApplyResult{}, c.dialErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return ApplyResult{}, fmt.Errorf("control: POST %s: status %d: %s", u, resp.StatusCode, bytes.TrimSpace(msg))
	}
	var res ApplyResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return ApplyResult{}, fmt.Errorf("decode response: %w", err)
	}
	return res, nil
}

// Exec runs cmd inside a live instance of svc, docker/kubectl-exec style. With
// tty it requests an interactive PTY: the caller is expected to have put the
// local terminal in raw mode and pass it as stdin/stdout (a single raw bidi
// stream carries the session, the server multiplexes the PTY). Without tty,
// stdin is streamed in and stdout/stderr stream back; the exit code is read from
// a response header. rows/cols seed the initial window for the tty path (0 = a
// default). Returns the exec'd process exit code.
//
// The tty path bypasses c.http and speaks HTTP/1.1 on a raw UDS connection so it
// can take the socket over after the response headers (connection hijack, the
// standard docker/k8s mechanism). A service/instance that cannot be resolved is
// reported by the server BEFORE the takeover (4xx), so it surfaces as an error.
func (c *Client) Exec(ctx context.Context, svc string, cmd []string, tty bool, rows, cols uint16, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if tty {
		return c.execTTY(ctx, svc, cmd, rows, cols, stdin, stdout)
	}
	return c.execStreamed(ctx, svc, cmd, stdin, stdout, stderr)
}

// execStreamed runs the non-interactive exec over the normal HTTP client: it
// posts the command, streams stdin as the request body, copies the response
// body to stdout, and reads the exit code from the response header.
func (c *Client) execStreamed(ctx context.Context, svc string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	body, err := json.Marshal(ExecRequest{Cmd: cmd})
	if err != nil {
		return -1, err
	}
	u := c.route(RouteExec) + url.PathEscape(svc)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+u, bytes.NewReader(body))
	if err != nil {
		return -1, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return -1, c.dialErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return -1, fmt.Errorf("service %q has no running instance in space %q", svc, c.space)
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return -1, fmt.Errorf("control: POST %s: status %d: %s", u, resp.StatusCode, bytes.TrimSpace(msg))
	}
	if _, err := io.Copy(stdout, resp.Body); err != nil {
		return -1, err
	}
	if v := resp.Trailer.Get(HeaderExecExitCode); v != "" {
		if code, cerr := strconv.Atoi(v); cerr == nil {
			return code, nil
		}
	}
	return 0, nil
}

// execTTY runs the interactive exec over a hijacked raw connection — a UDS conn
// locally, a TLS conn over the network (dialRaw abstracts the two). It writes the
// request line + headers (+ Authorization over the network) + JSON body, reads the
// status line/headers, and on 101/200 takes the stream over: stdin -> conn and
// conn -> stdout run until the server closes the stream (the exec ended). A
// non-2xx/101 status before takeover is surfaced as an error.
func (c *Client) execTTY(ctx context.Context, svc string, cmd []string, rows, cols uint16, stdin io.Reader, stdout io.Writer) (int, error) {
	conn, err := c.dialRaw(ctx)
	if err != nil {
		return -1, c.dialErr(err)
	}
	defer conn.Close()

	body, err := json.Marshal(ExecRequest{Cmd: cmd})
	if err != nil {
		return -1, err
	}
	u := fmt.Sprintf("%s%s?tty=1&rows=%d&cols=%d", c.route(RouteExec), url.PathEscape(svc), rows, cols)
	reqLine := fmt.Sprintf("POST %s HTTP/1.1\r\nHost: %s\r\n%sContent-Type: application/json\r\nContent-Length: %d\r\nConnection: upgrade\r\n\r\n", u, c.rawHost, c.rawAuth, len(body))
	if _, err := conn.Write([]byte(reqLine)); err != nil {
		return -1, err
	}
	if _, err := conn.Write(body); err != nil {
		return -1, err
	}

	// Read the status line + headers off the raw connection before the takeover.
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodPost})
	if err != nil {
		return -1, fmt.Errorf("read exec response: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols && resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if resp.StatusCode == http.StatusNotFound {
			return -1, fmt.Errorf("service %q has no running instance in space %q", svc, c.space)
		}
		return -1, fmt.Errorf("control: POST exec %q: status %d: %s", svc, resp.StatusCode, bytes.TrimSpace(msg))
	}

	// Hijack: proxy the raw bidi stream. conn -> stdout is the PTY output; stdin
	// -> conn is the user's keystrokes. The session ends when the server closes
	// the connection (exec exited); copying from br drains any buffered bytes.
	go func() { _, _ = io.Copy(conn, stdin) }()
	_, _ = io.Copy(stdout, br)
	return 0, nil
}

// getJSON does a GET with a short deadline and decodes a JSON body.
func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return c.dialErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("control: GET %s: status %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// dialErr maps a transport-level "socket absent / connection refused" to
// ErrNoSupervisor so the CLI can give an honest message; other errors pass
// through unchanged.
func (c *Client) dialErr(err error) error {
	if err == nil {
		return nil
	}
	if isNoSupervisor(err) {
		return fmt.Errorf("%w (space %q): %v", ErrNoSupervisor, c.space, err)
	}
	return err
}

// isNoSupervisor reports whether err is a missing socket file (ENOENT) or a
// refused connection (ECONNREFUSED, e.g. a stale/orphan socket nobody accepts)
// — both meaning "no supervisor is listening for this space".
func isNoSupervisor(err error) bool {
	return errors.Is(err, ErrNoSupervisor) ||
		errors.Is(err, os.ErrNotExist) ||
		errors.Is(err, syscall.ENOENT) ||
		errors.Is(err, syscall.ECONNREFUSED)
}
