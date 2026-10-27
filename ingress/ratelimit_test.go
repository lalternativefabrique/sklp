package ingress

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

var errTestDial = errors.New("test: dial disabled")

func TestRateLimiter_NilIsUnlimited(t *testing.T) {
	var r *rateLimiter // newRateLimiter returns nil for an unlimited route
	for i := 0; i < 1000; i++ {
		if !r.allow("1.2.3.4") {
			t.Fatal("nil limiter must always allow")
		}
	}
}

func TestRateLimiter_DisabledSpec(t *testing.T) {
	if newRateLimiter(RateSpec{}) != nil {
		t.Fatal("zero spec must yield a nil (unlimited) limiter")
	}
	if newRateLimiter(RateSpec{RPS: 0, Burst: 5}) != nil {
		t.Fatal("RPS 0 must yield a nil limiter")
	}
}

func TestRateLimiter_BurstThenThrottle(t *testing.T) {
	now := time.Unix(0, 0)
	r := newRateLimiter(RateSpec{RPS: 1, Burst: 3})
	r.now = func() time.Time { return now }

	// Burst of 3 allowed instantly.
	for i := 0; i < 3; i++ {
		if !r.allow("10.0.0.1") {
			t.Fatalf("request %d within burst must pass", i)
		}
	}
	// 4th within the same instant is throttled.
	if r.allow("10.0.0.1") {
		t.Fatal("4th request must be rate limited (burst exhausted)")
	}
	// After 1s one token refills (RPS 1).
	now = now.Add(time.Second)
	if !r.allow("10.0.0.1") {
		t.Fatal("a token should have refilled after 1s")
	}
	if r.allow("10.0.0.1") {
		t.Fatal("only one token refilled; second must throttle")
	}
}

func TestRateLimiter_PerIPIsolation(t *testing.T) {
	now := time.Unix(0, 0)
	r := newRateLimiter(RateSpec{RPS: 1, Burst: 1})
	r.now = func() time.Time { return now }

	if !r.allow("10.0.0.1") {
		t.Fatal("first IP first request must pass")
	}
	if r.allow("10.0.0.1") {
		t.Fatal("first IP second request throttled")
	}
	// A different IP has its own bucket.
	if !r.allow("10.0.0.2") {
		t.Fatal("second IP must not be affected by the first IP's bucket")
	}
}

func TestRateLimiter_EvictsIdle(t *testing.T) {
	now := time.Unix(0, 0)
	r := newRateLimiter(RateSpec{RPS: 1, Burst: 1})
	r.now = func() time.Time { return now }

	r.allow("10.0.0.1")
	if len(r.byIP) != 1 {
		t.Fatalf("expected 1 bucket, got %d", len(r.byIP))
	}
	// Advance past the TTL; the next allow (a different IP) triggers a sweep that
	// drops the idle bucket.
	now = now.Add(limiterIdleTTL + time.Second)
	r.allow("10.0.0.2")
	if _, stale := r.byIP["10.0.0.1"]; stale {
		t.Fatal("idle bucket should have been evicted")
	}
}

// TestHandler_RateLimit429 wires a limiter through the real handler and asserts
// the 429 on the over-limit request.
func TestHandler_RateLimit429(t *testing.T) {
	h := New()
	// Fail the dial instantly so the test never waits on real DNS/connect.
	h.WithDialer(func(context.Context, string, string) (net.Conn, error) {
		return nil, errTestDial
	})
	route := Route{Host: "app.example.com", Backend: "b:80", Rate: RateSpec{RPS: 1, Burst: 1}}
	h.SetRoutes([]Route{route})
	// Freeze the limiter clock so the (slow) proxy dial between the two requests
	// does not refill the bucket and mask the throttle.
	now := time.Unix(0, 0)
	h.limiterFor(route).now = func() time.Time { return now }

	req := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "http://app.example.com/", nil)
		r.RemoteAddr = "203.0.113.7:5000"
		return r
	}
	// First passes the limiter (then fails to dial nowhere:80 — but NOT 429).
	w1 := httptest.NewRecorder()
	h.ServeHTTP(w1, req())
	if w1.Code == http.StatusTooManyRequests {
		t.Fatal("first request must not be rate limited")
	}
	// Second from the same IP is throttled at the limiter, before proxying.
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req())
	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request must be 429, got %d", w2.Code)
	}
}

// TestHandler_RateLimitReusedAcrossReload proves an unchanged route keeps its
// live per-IP buckets across a SetRoutes reload (so a reload does not reset the
// limit).
func TestHandler_RateLimitReusedAcrossReload(t *testing.T) {
	h := New()
	route := Route{Host: "app.example.com", Backend: "b:80", Rate: RateSpec{RPS: 1, Burst: 1}}
	h.SetRoutes([]Route{route})
	first := h.limiterFor(route)
	// Reload with the SAME route + rate: the limiter instance must be reused.
	h.SetRoutes([]Route{route})
	if h.limiterFor(route) != first {
		t.Fatal("unchanged route must keep its live limiter across reload")
	}
	// Reload with a CHANGED rate: a new limiter is built.
	route2 := route
	route2.Rate = RateSpec{RPS: 5, Burst: 10}
	h.SetRoutes([]Route{route2})
	if h.limiterFor(route2) == first {
		t.Fatal("changed rate must rebuild the limiter")
	}
}
