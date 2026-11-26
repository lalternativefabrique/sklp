package tags

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client lists the tags of an OCI repository.
//
// It discovers the token endpoint from the registry's own 401 challenge rather
// than hardcoding one, because registries do not agree on where it lives:
// Docker Hub answers from auth.docker.io, while Scaleway's rg.fr-par.scw.cloud
// points at api.scaleway.com/registry-internal — a client assuming the /v2/token
// convention gets a 404 there and reads it as "no tags".
type Client struct {
	http *http.Client
	// baseScheme is https in production; tests point it at an httptest server.
	baseScheme string
}

func NewClient() *Client {
	return &Client{
		http:       &http.Client{Timeout: 30 * time.Second},
		baseScheme: "https",
	}
}

// ListTags returns every tag of repo ("host/namespace/name"). username and
// secret authenticate the token exchange; both empty means an anonymous pull.
func (c *Client) ListTags(ctx context.Context, repo, username, secret string) ([]string, error) {
	host, path, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	endpoint := c.baseScheme + "://" + host + "/v2/" + path + "/tags/list"

	resp, err := c.get(ctx, endpoint, "")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("WWW-Authenticate")
		resp.Body.Close()
		token, terr := c.fetchToken(ctx, challenge, path, username, secret)
		if terr != nil {
			return nil, terr
		}
		if resp, err = c.get(ctx, endpoint, token); err != nil {
			return nil, err
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list tags for %s: status %d", repo, resp.StatusCode)
	}
	var body struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode tag list for %s: %w", repo, err)
	}
	return body.Tags, nil
}

func (c *Client) get(ctx context.Context, endpoint, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/json")
	return c.http.Do(req)
}

// fetchToken performs the token exchange the challenge describes, scoped to a
// pull on this repository alone — a broader scope would mint a credential with
// more reach than the watcher needs.
func (c *Client) fetchToken(ctx context.Context, challenge, repoPath, username, secret string) (string, error) {
	realm, service := parseChallenge(challenge)
	if realm == "" {
		return "", fmt.Errorf("registry requires auth but advertised no realm")
	}
	u, err := url.Parse(realm)
	if err != nil {
		return "", fmt.Errorf("parse auth realm %q: %w", realm, err)
	}
	q := u.Query()
	if service != "" {
		q.Set("service", service)
	}
	q.Set("scope", "repository:"+repoPath+":pull")
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	if username != "" || secret != "" {
		req.SetBasicAuth(username, secret)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// The status alone diagnoses this: a 401 is a wrong credential, a 403 a
		// credential without pull rights. The body is not propagated because this
		// error is stored on the watch row and rendered in the UI, and an auth
		// endpoint may echo the request — including the secret it just rejected.
		return "", fmt.Errorf("registry auth: status %d", resp.StatusCode)
	}
	// Registries disagree on the field name: the OCI spec says "token", Docker
	// Hub also sends "access_token". Accept either.
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("decode auth response: %w", err)
	}
	if body.Token != "" {
		return body.Token, nil
	}
	if body.AccessToken != "" {
		return body.AccessToken, nil
	}
	return "", fmt.Errorf("registry auth returned no token")
}

// parseChallenge extracts realm and service from a Bearer WWW-Authenticate
// header, e.g. `Bearer realm="https://auth.example.com/token",service="registry"`.
func parseChallenge(header string) (realm, service string) {
	rest, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return "", ""
	}
	for _, part := range splitParams(rest) {
		key, val, found := strings.Cut(part, "=")
		if !found {
			continue
		}
		val = strings.Trim(strings.TrimSpace(val), `"`)
		switch strings.TrimSpace(key) {
		case "realm":
			realm = val
		case "service":
			service = val
		}
	}
	return realm, service
}

// splitParams splits on commas that are not inside a quoted value, so a realm
// URL carrying a comma in a query string survives.
func splitParams(s string) []string {
	var out []string
	var cur strings.Builder
	inQuotes := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuotes = !inQuotes
			cur.WriteRune(r)
		case r == ',' && !inQuotes:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// splitRepo separates the registry host from the repository path. A reference
// without a host is rejected rather than defaulting to Docker Hub: a watch is
// always explicit about where it pulls from.
func splitRepo(repo string) (host, path string, err error) {
	host, path, found := strings.Cut(strings.TrimSpace(repo), "/")
	if !found || path == "" {
		return "", "", fmt.Errorf("image repository %q must be host/namespace/name", repo)
	}
	if !strings.Contains(host, ".") && !strings.Contains(host, ":") && host != "localhost" {
		return "", "", fmt.Errorf("image repository %q must name a registry host", repo)
	}
	return host, path, nil
}
