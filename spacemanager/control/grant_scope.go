//go:build linux

package control

import (
	"net/http"
	"strings"

	"github.com/lalternativefabrique/sklp/spacemanager/grants"
)

// neverGranted are the node's own keys: no grant reaches them, admin included.
var neverGranted = []string{
	RouteTokenRotate,
	RouteMeshPeers,
	RouteGateway,
	RoutePublicNIC,
}

// GrantAllows answers whether a request a granted service sends over its
// socket may reach the control API. path is the wire path of the multi-space
// API ("/v1/spaces/<space>/state", "/v1/follows", ...).
func GrantAllows(scope grants.Scope, method, path string) bool {
	if strings.Contains(path, "..") {
		return false
	}
	for _, p := range neverGranted {
		if path == p || strings.HasPrefix(path, p+"/") {
			return false
		}
	}
	rest := ""
	if space, r, ok := splitSpacePath(path); ok && space != "" {
		rest = r
		if strings.HasPrefix(rest, strings.TrimPrefix(RouteDial, "/v1")) || strings.HasPrefix(rest, strings.TrimPrefix(RouteGatewayRoutes, "/v1")) {
			return false
		}
	}
	if readable(method, path, rest) {
		return true
	}
	switch scope {
	case grants.Admin:
		return true
	case grants.Operate:
		return operable(method, path, rest)
	default:
		return false
	}
}

func readable(method, path, rest string) bool {
	if method != http.MethodGet {
		return false
	}
	switch path {
	case RouteHealth, RouteSpaces, RouteFollows, RouteWatches, RouteRestores:
		return true
	}
	if rest == "" {
		return false
	}
	return rest == "/state" || rest == "/manifest" || rest == "/datastores" ||
		strings.HasPrefix(rest, strings.TrimPrefix(RouteLogs, "/v1"))
}

func operable(method, path, rest string) bool {
	switch {
	case path == RouteFollows && method == http.MethodPost:
		return true
	case strings.HasPrefix(path, RouteFollows+"/") && method == http.MethodDelete:
		return true
	case rest != "" && method == http.MethodPost && strings.HasPrefix(rest, strings.TrimPrefix(RouteRestart, "/v1")):
		return true
	}
	return false
}
