//go:build linux

package control

import (
	"net/http"
	"path"
	"strings"

	"github.com/lalternativefabrique/sklp/spacemanager/grants"
)

type grantRoute struct {
	method string
	route  string
	prefix bool
	scope  grants.Scope
}

// Every route a grant may reach is listed here; anything else is refused at
// every scope. The node's own keys (token rotation, mesh peers, gateway
// control, the public NIC) and raw dials into services are absent on purpose,
// including through a space prefix, where the per-space mux serves some of
// them too.
var nodeGrantRoutes = []grantRoute{
	{http.MethodGet, RouteHealth, false, grants.Read},
	{http.MethodGet, RouteSpaces, false, grants.Read},
	{http.MethodGet, RouteRestores, false, grants.Read},
	{http.MethodGet, RouteWatches, false, grants.Read},
	{http.MethodGet, RouteFollows, false, grants.Read},
	{http.MethodPost, RouteFollows, false, grants.Operate},
	{http.MethodDelete, RouteFollows + "/", true, grants.Operate},
}

var spaceGrantRoutes = []grantRoute{
	{http.MethodGet, RouteHealth, false, grants.Read},
	{http.MethodGet, RouteState, false, grants.Read},
	{http.MethodGet, RouteManifest, false, grants.Read},
	{http.MethodGet, RouteDataStores, false, grants.Read},
	{http.MethodGet, RouteLogs, true, grants.Read},
	{http.MethodPost, RouteRestart, true, grants.Operate},
	{http.MethodPost, RouteApply, false, grants.Admin},
	{http.MethodPost, RouteImage, false, grants.Admin},
	{http.MethodPost, RouteDown, false, grants.Admin},
	{http.MethodPost, RouteStart, false, grants.Admin},
	{http.MethodPost, RouteRun, true, grants.Admin},
	{http.MethodPost, RouteExec, true, grants.Admin},
}

var scopeRank = map[grants.Scope]int{grants.Read: 1, grants.Operate: 2, grants.Admin: 3}

// GrantAllows answers whether a request a granted service sends over its
// socket may reach the control API. path is the wire path of the multi-space
// API ("/v1/spaces/<space>/state", "/v1/follows", ...); a path that is not
// already canonical is refused rather than interpreted.
func GrantAllows(scope grants.Scope, method, p string) bool {
	if p == "" || path.Clean(p) != p && path.Clean(p)+"/" != p {
		return false
	}
	routes, rest := nodeGrantRoutes, p
	if space, r, ok := splitSpacePath(p); ok {
		if !grants.ValidName(space) {
			return false
		}
		routes, rest = spaceGrantRoutes, "/v1"+r
	}
	for _, gr := range routes {
		if gr.method != method {
			continue
		}
		hit := rest == gr.route
		if gr.prefix {
			name := strings.TrimPrefix(rest, gr.route)
			hit = name != rest && name != "" && !strings.Contains(name, "/")
		}
		if hit {
			return scopeRank[scope] >= scopeRank[gr.scope]
		}
	}
	return false
}
