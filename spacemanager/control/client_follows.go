package control

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func (c *Client) Follows(ctx context.Context) (FollowsResponse, error) {
	var out FollowsResponse
	if err := c.getJSON(ctx, RouteFollows, &out); err != nil {
		return FollowsResponse{}, err
	}
	return out, nil
}

// Follow asks the node to follow a repo. The node's refusal is returned as
// it phrased it: it says which repo or name it would not take, and why.
func (c *Client) Follow(ctx context.Context, req FollowRequest) (FollowEntry, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return FollowEntry{}, err
	}
	resp, err := c.followCall(ctx, http.MethodPost, RouteFollows, bytes.NewReader(body))
	if err != nil {
		return FollowEntry{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return FollowEntry{}, followFailure(resp)
	}
	var out FollowEntry
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return FollowEntry{}, err
	}
	return out, nil
}

func (c *Client) Unfollow(ctx context.Context, name string) error {
	resp, err := c.followCall(ctx, http.MethodDelete, RouteFollows+"/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return followFailure(resp)
	}
	return nil
}

func (c *Client) followCall(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		cancel()
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		cancel()
		return nil, c.dialErr(err)
	}
	resp.Body = cancelReadCloser{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

func followFailure(resp *http.Response) error {
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	text := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(msg)), "follows:"))
	if resp.StatusCode == http.StatusNotImplemented {
		return fmt.Errorf("this node does not reconcile from repos (needs sklet >= v0.2.11)")
	}
	if text == "" {
		text = fmt.Sprintf("status %d", resp.StatusCode)
	}
	return fmt.Errorf("%s", text)
}

type cancelReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c cancelReadCloser) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}
