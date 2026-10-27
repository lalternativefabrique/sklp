package ingress

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// ratelimit.go is the native L7's per-route, per-client-IP rate limiter — the
// piece traefik's rateLimit middleware would provide, done in-process. Each rate
// limited route owns a token-bucket PER client IP (golang.org/x/time/rate):
// requests over the sustained rate (after the burst allowance) get a 429. Buckets
// for idle IPs are evicted so a scan of many source IPs cannot grow the map
// unbounded. Local to the worker (per space) — no shared store, which is the
// right scope for a single-node space.

// RateSpec is a route's rate-limit config: RPS sustained requests per second per
// client IP, Burst the bucket size (max instantaneous requests). A zero RateSpec
// (RPS == 0) means unlimited — the limiter is not installed for that route.
type RateSpec struct {
	RPS   float64 `json:"rps,omitempty"`
	Burst int     `json:"burst,omitempty"`
}

// enabled reports whether the spec imposes a limit.
func (s RateSpec) enabled() bool { return s.RPS > 0 }

// limiterIdleTTL is how long an IP's bucket may sit unused before eviction. A
// bucket at full tokens is equivalent to a fresh one, so dropping it loses no
// state a returning client would notice.
const limiterIdleTTL = 10 * time.Minute

// ipLimiter is one client IP's bucket plus its last-seen time (for eviction).
type ipLimiter struct {
	lim  *rate.Limiter
	seen time.Time
}

// rateLimiter holds per-client-IP token buckets for ONE route's RateSpec. Safe
// for concurrent use. Build with newRateLimiter; call allow(ip) per request.
type rateLimiter struct {
	spec RateSpec
	now  func() time.Time // injectable for deterministic tests

	mu    sync.Mutex
	byIP  map[string]*ipLimiter
	swept time.Time
}

// newRateLimiter builds a limiter for spec, or nil when the spec imposes no
// limit (so callers can skip the check with a nil test).
func newRateLimiter(spec RateSpec) *rateLimiter {
	if !spec.enabled() {
		return nil
	}
	return &rateLimiter{spec: spec, now: time.Now, byIP: map[string]*ipLimiter{}}
}

// allow reports whether a request from ip may proceed, consuming one token. A
// nil limiter always allows (unlimited route). Unknown/blank ip shares a single
// bucket so a client sending no usable address is still bounded.
func (r *rateLimiter) allow(ip string) bool {
	if r == nil {
		return true
	}
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweepLocked(now)
	l := r.byIP[ip]
	if l == nil {
		l = &ipLimiter{lim: rate.NewLimiter(rate.Limit(r.spec.RPS), r.spec.Burst)}
		r.byIP[ip] = l
	}
	l.seen = now
	return l.lim.AllowN(now, 1)
}

// sweepLocked evicts idle buckets, at most once per TTL window so the scan cost
// is amortised. Caller holds r.mu.
func (r *rateLimiter) sweepLocked(now time.Time) {
	if now.Sub(r.swept) < limiterIdleTTL {
		return
	}
	r.swept = now
	for ip, l := range r.byIP {
		if now.Sub(l.seen) >= limiterIdleTTL {
			delete(r.byIP, ip)
		}
	}
}
