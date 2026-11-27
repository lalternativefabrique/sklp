package control

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lalternativefabrique/sklp/space"
	"github.com/lalternativefabrique/sklp/spacemanager/grants"
)

func TestGrantScopes(t *testing.T) {
	cases := []struct {
		scope        grants.Scope
		method, path string
		want         bool
	}{
		{grants.Read, "GET", "/v1/spaces", true},
		{grants.Read, "GET", "/v1/follows", true},
		{grants.Read, "GET", "/v1/watches", true},
		{grants.Read, "GET", "/v1/spaces/hello/state", true},
		{grants.Read, "GET", "/v1/spaces/hello/manifest", true},
		{grants.Read, "GET", "/v1/spaces/hello/logs/web", true},
		{grants.Read, "POST", "/v1/follows", false},
		{grants.Read, "POST", "/v1/spaces/hello/restart/web", false},
		{grants.Read, "POST", "/v1/spaces/hello/apply", false},
		{grants.Operate, "POST", "/v1/follows", true},
		{grants.Operate, "DELETE", "/v1/follows/hello", true},
		{grants.Operate, "POST", "/v1/spaces/hello/restart/web", true},
		{grants.Operate, "POST", "/v1/spaces/hello/apply", false},
		{grants.Operate, "POST", "/v1/spaces/hello/exec/web", false},
		{grants.Operate, "POST", "/v1/spaces/hello/down", false},
		{grants.Admin, "POST", "/v1/spaces/hello/apply", true},
		{grants.Admin, "POST", "/v1/spaces/hello/exec/web", true},
		{grants.Admin, "POST", "/v1/token/rotate", false},
		{grants.Admin, "GET", "/v1/mesh/peers", false},
		{grants.Admin, "DELETE", "/v1/mesh/peers/x", false},
		{grants.Admin, "GET", "/v1/gateway", false},
		{grants.Admin, "POST", "/v1/gateway/up", false},
		{grants.Admin, "GET", "/v1/host/public-nic", false},
		{grants.Admin, "POST", "/v1/spaces/hello/dial/db", false},
		{grants.Admin, "PUT", "/v1/spaces/hello/gateway/routes", false},
		{grants.Admin, "GET", "/v1/spaces/../token/rotate", false},
		{grants.Admin, "GET", "/v1/spaces/hello/host/public-nic", false},
		{grants.Admin, "GET", "/v1/spaces/hello/spaces", false},
		{grants.Admin, "GET", "/v1/spaces/hello/restores", false},
		{grants.Admin, "POST", "//v1/token/rotate", false},
		{grants.Admin, "GET", "/v1//follows", false},
		{grants.Admin, "GET", "/v1/spaces/Hello/state", false},
		{grants.Admin, "GET", "/v1/spaces/hello/logs/", false},
		{grants.Admin, "GET", "/v1/spaces/hello/logs/web/../../apply", false},
		{grants.Admin, "DELETE", "/v1/follows/", false},
		{grants.Admin, "GET", "/v1/unknown", false},
		{grants.Admin, "PUT", "/v1/spaces/hello/apply", false},
		{grants.Admin, "POST", "/v1/spaces/hello/start", true},
		{grants.Admin, "POST", "/v1/spaces/hello/run/migrate", true},
	}
	for _, c := range cases {
		if got := GrantAllows(c.scope, c.method, c.path); got != c.want {
			t.Errorf("%s %s %s = %v, want %v", c.scope, c.method, c.path, got, c.want)
		}
	}
}

func TestGrantSocketAnswersOnlyForTheOwnerItWasGivenTo(t *testing.T) {
	xdg, err := os.MkdirTemp("", "g")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(xdg) })
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(xdg, "c"))
	seedManifestOwner(t, xdg, "playground", "playground/prod")
	g := grants.Grant{Space: "playground", Service: "core", Owner: "playground/prod", Scope: grants.Read}
	if err := grants.Put(g); err != nil {
		t.Fatal(err)
	}

	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "api "+r.URL.Path) })
	live := map[string]*grantListener{}
	t.Cleanup(func() {
		for _, l := range live {
			_ = l.srv.Close()
		}
	})
	dir, err := space.ControlSocketDir("playground", "core")
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, space.ControlSocketName)
	syncGrantListeners(live, api)
	if len(live) != 0 {
		t.Fatal("no socket before the space exists: its state dir is the space's, never the agent's to create")
	}
	spaceDir := filepath.Dir(filepath.Dir(dir))
	if err := os.MkdirAll(spaceDir, 0o710); err != nil {
		t.Fatal(err)
	}
	syncGrantListeners(live, api)
	if st, err := os.Stat(sock); err != nil || st.Mode().Perm() != 0o666 {
		t.Fatalf("socket: %v %v", st, err)
	}
	if st, err := os.Stat(filepath.Dir(dir)); err != nil || st.Mode().Perm() != 0o711 {
		t.Fatalf("control dir may be traversed, never listed: %v %v", st, err)
	}
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
	call := func(method, path string) (int, string) {
		req, _ := http.NewRequest(method, "http://grant"+path, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	if code, body := call("GET", "/v1/spaces"); code != 200 || body != "api /v1/spaces" {
		t.Fatalf("read within scope: %d %q", code, body)
	}
	if code, _ := call("POST", "/v1/follows"); code != http.StatusForbidden {
		t.Fatalf("write outside a read scope: %d", code)
	}

	seedManifestOwner(t, xdg, "playground", "someone-else/web")
	if code, body := call("GET", "/v1/spaces"); code != http.StatusForbidden || !strings.Contains(body, "no longer") {
		t.Fatalf("a space recreated by another owner inherits nothing: %d %q", code, body)
	}

	if _, err := grants.Remove("playground", "core"); err != nil {
		t.Fatal(err)
	}
	syncGrantListeners(live, api)
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("a revoked grant's socket must go: %v", err)
	}
}
