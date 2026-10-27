// Package apiclient is a thin authenticated HTTP wrapper used by the
// sklp CLI to talk to the Skalpel core API. Every request gets an
// Authorization: Bearer <token> header (when a token is configured),
// JSON content-type if a body is provided, and a default 10s timeout.
package apiclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultTimeout caps any single request. The CLI is interactive — we
// prefer a clear timeout error to a hanging shell.
const DefaultTimeout = 10 * time.Second

// Client is an authenticated HTTP client targeting a single Skalpel API.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// New returns a ready-to-use Client. Passes through to http.DefaultClient
// with a 10s timeout if no custom client is provided later.
func New(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: DefaultTimeout},
	}
}

// APIError is returned for non-2xx responses. The raw body is preserved
// so callers can include it in user-facing error messages.
type APIError struct {
	StatusCode int
	Body       []byte
}

func (e *APIError) Error() string {
	return fmt.Sprintf("api error: status %d: %s", e.StatusCode, string(e.Body))
}

// Do performs an authenticated request. body, if non-nil, is JSON-encoded
// and Content-Type is set accordingly. Returns the raw response body and
// status code. On non-2xx the error wraps an *APIError so callers can
// inspect StatusCode without losing the body.
func (c *Client) Do(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var reader io.Reader
	hasBody := false
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("marshal body: %w", err)
		}
		reader = bytes.NewReader(buf)
		hasBody = true
	}

	url := c.BaseURL + path
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: DefaultTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("http %s %s: %w", method, url, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return respBody, resp.StatusCode, &APIError{StatusCode: resp.StatusCode, Body: respBody}
	}
	return respBody, resp.StatusCode, nil
}

// Get is a convenience wrapper for GET.
func (c *Client) Get(ctx context.Context, path string) ([]byte, int, error) {
	return c.Do(ctx, http.MethodGet, path, nil)
}

// Post is a convenience wrapper for POST.
func (c *Client) Post(ctx context.Context, path string, body any) ([]byte, int, error) {
	return c.Do(ctx, http.MethodPost, path, body)
}

// Delete is a convenience wrapper for DELETE.
func (c *Client) Delete(ctx context.Context, path string) ([]byte, int, error) {
	return c.Do(ctx, http.MethodDelete, path, nil)
}

// Patch is a convenience wrapper for PATCH.
func (c *Client) Patch(ctx context.Context, path string, body any) ([]byte, int, error) {
	return c.Do(ctx, http.MethodPatch, path, body)
}

// SSEEvent is one `event: <name>\ndata: <payload>\n\n` frame from a
// text/event-stream response.
type SSEEvent struct {
	Name string
	Data []byte
}

// StreamSSE opens a GET request against path and decodes it as
// text/event-stream, sending each frame to the returned channel. The
// channel closes when the server ends the stream, ctx is cancelled, or a
// read error occurs (surfaced via the returned error channel first).
func (c *Client) StreamSSE(ctx context.Context, path string) (<-chan SSEEvent, <-chan error, error) {
	url := c.BaseURL + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("build request: %w", err)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	req.Header.Set("Accept", "text/event-stream")

	client := c.HTTP
	if client == nil {
		client = &http.Client{}
	}
	// SSE is long-lived; the default client timeout would kill the stream.
	streamClient := *client
	streamClient.Timeout = 0

	resp, err := streamClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("http GET %s: %w", url, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, nil, &APIError{StatusCode: resp.StatusCode, Body: body}
	}

	events := make(chan SSEEvent)
	errc := make(chan error, 1)
	go func() {
		defer resp.Body.Close()
		defer close(events)
		defer close(errc)

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

		var eventName string
		var dataLines [][]byte
		flush := func() {
			if len(dataLines) == 0 && eventName == "" {
				return
			}
			select {
			case events <- SSEEvent{Name: eventName, Data: bytes.Join(dataLines, []byte("\n"))}:
			case <-ctx.Done():
			}
			eventName = ""
			dataLines = nil
		}
		for scanner.Scan() {
			line := scanner.Bytes()
			switch {
			case len(line) == 0:
				flush()
			case bytes.HasPrefix(line, []byte("event:")):
				eventName = strings.TrimSpace(string(bytes.TrimPrefix(line, []byte("event:"))))
			case bytes.HasPrefix(line, []byte("data:")):
				dataLines = append(dataLines, bytes.TrimPrefix(line, []byte("data:")))
			}
		}
		flush()
		if err := scanner.Err(); err != nil {
			errc <- err
		}
	}()
	return events, errc, nil
}

// PostRaw performs an authenticated POST with a caller-provided body and
// Content-Type. Used for non-JSON payloads (e.g. multipart/form-data).
func (c *Client) PostRaw(ctx context.Context, path, contentType string, body io.Reader) ([]byte, int, error) {
	url := c.BaseURL + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")

	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: DefaultTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("http POST %s: %w", url, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return respBody, resp.StatusCode, &APIError{StatusCode: resp.StatusCode, Body: respBody}
	}
	return respBody, resp.StatusCode, nil
}
