//go:build linux

package control

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/lalternativefabrique/sklp/space"
	"github.com/lalternativefabrique/sklp/spacemanager/grants"
)

const grantReload = 5 * time.Second

type grantListener struct {
	grant grants.Grant
	srv   *http.Server
	sock  string
}

// ServeGrants keeps one Unix socket per grant, each answering the control API
// for that grant alone: no TLS and no token, the socket is the credential,
// reachable only from the granted service whose container mounts its
// directory. It follows the grants file as the operator edits it.
func ServeGrants(ctx context.Context, reg *Registry, materialize Materializer) {
	api := newMultiMuxWithTokens(reg, materialize, nil)
	live := map[string]*grantListener{}
	defer func() {
		for _, l := range live {
			_ = l.srv.Close()
		}
	}()
	tick := time.NewTicker(grantReload)
	defer tick.Stop()
	for {
		syncGrantListeners(live, api)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func syncGrantListeners(live map[string]*grantListener, api http.Handler) {
	list, err := grants.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "sklet: control grants: %v\n", err)
		return
	}
	wanted := map[string]grants.Grant{}
	for _, g := range list {
		wanted[g.Key()] = g
	}
	for key, l := range live {
		if g, ok := wanted[key]; !ok || g != l.grant || !socketStillThere(l.sock) {
			_ = l.srv.Close()
			_ = os.Remove(l.sock)
			delete(live, key)
		}
	}
	for key, g := range wanted {
		if _, ok := live[key]; ok {
			continue
		}
		l, err := listenGrant(g, api)
		if errors.Is(err, errSpaceNotUp) {
			continue
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "sklet: control grant %s: %v\n", key, err)
			continue
		}
		live[key] = l
	}
}

var errSpaceNotUp = errors.New("space not materialised yet")

// socketStillThere notices a space wiped and rebuilt under the agent: its
// state dir, and the socket in it, are gone, so the listener must be redone.
func socketStillThere(sock string) bool {
	_, err := os.Lstat(sock)
	return err == nil
}

func listenGrant(g grants.Grant, api http.Handler) (*grantListener, error) {
	dir, err := space.ControlSocketDir(g.Space, g.Service)
	if err != nil {
		return nil, err
	}
	// The space's state dir is the space's own (0710, its child root's
	// group): only that space traverses it, which is what keeps every other
	// space and host account off the socket. It is never created here.
	controlDir := filepath.Dir(dir)
	if _, err := os.Stat(filepath.Dir(controlDir)); err != nil {
		return nil, errSpaceNotUp
	}
	if err := os.MkdirAll(controlDir, 0o711); err != nil {
		return nil, err
	}
	if err := os.Chmod(controlDir, 0o711); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		return nil, err
	}
	sock := filepath.Join(dir, space.ControlSocketName)
	if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	// The container runs under its own user namespace, where the agent's
	// uid is unmapped: only the "other" bits let it connect.
	if err := os.Chmod(sock, 0o666); err != nil {
		_ = ln.Close()
		return nil, err
	}
	srv := newControlServer(grantGuard(g, api))
	go func() { _ = srv.Serve(ln) }()
	fmt.Fprintf(os.Stdout, "sklet: control grant %s (%s) on %s\n", g.Key(), g.Scope, sock)
	return &grantListener{grant: g, srv: srv, sock: sock}, nil
}

func grantGuard(g grants.Grant, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if owner, ok := liveSpaceOwner(g.Space); !ok || owner != g.Owner {
			fmt.Fprintf(os.Stderr, "sklet: control grant %s refused %s %s: space %s is no longer owned by %s\n", g.Key(), r.Method, r.URL.Path, g.Space, g.Owner)
			http.Error(w, "grant: this space is no longer the one the grant was given to", http.StatusForbidden)
			return
		}
		if !GrantAllows(g.Scope, r.Method, r.URL.Path) {
			fmt.Fprintf(os.Stderr, "sklet: control grant %s refused %s %s: outside scope %s\n", g.Key(), r.Method, r.URL.Path, g.Scope)
			http.Error(w, fmt.Sprintf("grant: %s %s is outside the %s scope", r.Method, r.URL.Path, g.Scope), http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet {
			fmt.Fprintf(os.Stdout, "sklet: control grant %s: %s %s\n", g.Key(), r.Method, r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}
