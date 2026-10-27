package spacemanager

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// routekind.go turns a route's declared KIND into a rate limit. It exists
// because the alternative — guessing from the path — cannot work: an auth
// endpoint is as often /api/v2/sessions, /oauth/token or /graphql as it is
// /login, and a name-matching heuristic both misses those (the operator
// believes a limit is in place when none is) and fires on innocent paths
// (/authors matching /auth). What a route IS, only its owner knows; so the
// route declares it, and sklp supplies the numbers.
//
// The defaults are deliberately generous. A limit that trips on legitimate
// traffic is worse than no limit: it breaks real clients and gets switched off.

// RouteKind values accepted in `kind:`.
const (
	RouteKindAuth      = "auth"      // login, signup, password reset, token issuance
	RouteKindAPI       = "api"       // authenticated application API
	RouteKindWeb       = "web"       // pages and assets — bursty by nature
	RouteKindExpensive = "expensive" // export, search, upload: each request costs real work
)

// kindLimit is the rate limit a kind supplies when the route sets no explicit
// rate/burst.
type kindLimit struct {
	rate  float64
	burst int
}

// kindLimits maps each kind to its default. `web` is intentionally the loosest:
// one page load is a single HTML request plus dozens of parallel assets, so a
// tight limit there throttles ordinary browsing rather than abuse.
var kindLimits = map[string]kindLimit{
	RouteKindAuth:      {rate: 5, burst: 10},
	RouteKindAPI:       {rate: 50, burst: 100},
	RouteKindWeb:       {rate: 100, burst: 200},
	RouteKindExpensive: {rate: 2, burst: 5},
}

// knownRouteKinds lists the accepted kinds, sorted, for error messages.
func knownRouteKinds() []string {
	out := make([]string, 0, len(kindLimits))
	for k := range kindLimits {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// validateRouteKind rejects an unknown kind. An unrecognised value must fail
// loudly: silently ignoring it would leave the route unlimited while its
// author believes a limit applies — the failure mode this whole field exists
// to prevent.
func validateRouteKind(kind string) error {
	if kind == "" {
		return nil
	}
	if _, ok := kindLimits[kind]; !ok {
		return fmt.Errorf("unknown route kind %q (known: %s)", kind, strings.Join(knownRouteKinds(), ", "))
	}
	return nil
}

// limitFor resolves the effective (rate, burst) for a route: an explicit rate
// always wins, so a kind is a default and never a ceiling. Resolution happens
// ONCE, here, on the neutral model both front doors consume — a per-driver
// default would let the two diverge, which is exactly the class of bug that
// left rate limits unenforced on the traefik path.
//
// An explicit rate with no burst keeps burst 0; validateGateway rejects that
// pair, so the invalid combination is refused at parse time rather than
// silently repaired here.
func limitFor(kind string, rate float64, burst int) (float64, int) {
	if rate > 0 {
		return rate, burst
	}
	if l, ok := kindLimits[kind]; ok {
		return l.rate, l.burst
	}
	return rate, burst
}

// parseRouteTimeout turns a route's `timeout:` string into a duration. Empty
// (or an explicit "0") means unbounded — the default, since a route serving
// large transfers or a WebSocket must not inherit a cap it never asked for.
//
// A negative value is refused rather than clamped: it can only be a mistake,
// and silently reading it as "unbounded" would leave a route the operator
// believes is capped running without one.
func parseRouteTimeout(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid timeout %q (want a Go duration like \"30s\" or \"2m\"): %w", s, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("negative timeout %q", s)
	}
	return d, nil
}

// parseRouteMaxBody turns a route's `max_body:` into a byte cap, reusing the
// same Ki/Mi/Gi + MB vocabulary as `resources.memory` so a deploy.yaml carries
// one size syntax, not two.
//
// Empty means unbounded, and so does an explicit "0" — which parseMemory itself
// rejects, since a zero MEMORY limit is meaningless while a zero body cap is
// the documented way to say "no limit". Unbounded is the default on purpose: a
// cap inherited from anywhere would turn a working upload into a 413 that
// nobody declared.
func parseRouteMaxBody(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, nil
	}
	n, err := parseMemory(s)
	if err != nil {
		return 0, fmt.Errorf("invalid max_body %q (want bytes or a Ki/Mi/Gi suffix, or 0 for unlimited)", s)
	}
	return n, nil
}
