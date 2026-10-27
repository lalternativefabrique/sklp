package ingress

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// backend spins a tiny HTTP server that echoes a tag, standing in for a space
// service the ingress proxies to.
func backend(t *testing.T, tag string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, tag+":"+r.URL.Path)
	}))
	t.Cleanup(s.Close)
	return s
}

// hostOf strips the scheme from a test server URL → "127.0.0.1:port".
func hostOf(s *httptest.Server) string { return strings.TrimPrefix(s.URL, "http://") }

func TestIngress_RoutesByHost(t *testing.T) {
	a := backend(t, "A")
	b := backend(t, "B")
	h := New()
	h.SetRoutes([]Route{
		{Host: "a.test", Backend: hostOf(a)},
		{Host: "b.test", Backend: hostOf(b)},
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	if got := get(t, srv.URL, "a.test", "/"); !strings.HasPrefix(got, "A:") {
		t.Errorf("a.test -> %q, want A backend", got)
	}
	if got := get(t, srv.URL, "b.test", "/"); !strings.HasPrefix(got, "B:") {
		t.Errorf("b.test -> %q, want B backend", got)
	}
}

func TestIngress_RoutesByPath_LongestWins(t *testing.T) {
	root := backend(t, "ROOT")
	api := backend(t, "API")
	h := New()
	h.SetRoutes([]Route{
		{Host: "app.test", Path: "/", Backend: hostOf(root)},
		{Host: "app.test", Path: "/api", Backend: hostOf(api)},
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	if got := get(t, srv.URL, "app.test", "/api/users"); !strings.HasPrefix(got, "API:") {
		t.Errorf("/api/users -> %q, want API (longest prefix)", got)
	}
	if got := get(t, srv.URL, "app.test", "/home"); !strings.HasPrefix(got, "ROOT:") {
		t.Errorf("/home -> %q, want ROOT", got)
	}
	// "/apixyz" must NOT match the "/api" prefix (segment-aware).
	if got := get(t, srv.URL, "app.test", "/apixyz"); !strings.HasPrefix(got, "ROOT:") {
		t.Errorf("/apixyz -> %q, want ROOT (not /api)", got)
	}
}

func TestIngress_UnknownHost_404(t *testing.T) {
	a := backend(t, "A")
	h := New()
	h.SetRoutes([]Route{{Host: "a.test", Backend: hostOf(a)}})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest("GET", srv.URL+"/", nil)
	req.Host = "unknown.test"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown host status = %d, want 404", resp.StatusCode)
	}
}

func TestIngress_SetRoutes_HotSwap(t *testing.T) {
	a := backend(t, "A")
	b := backend(t, "B")
	h := New()
	h.SetRoutes([]Route{{Host: "x.test", Backend: hostOf(a)}})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	if got := get(t, srv.URL, "x.test", "/"); !strings.HasPrefix(got, "A:") {
		t.Errorf("before swap: %q, want A", got)
	}
	// Hot-swap the routing table; the same host now proxies to B.
	h.SetRoutes([]Route{{Host: "x.test", Backend: hostOf(b)}})
	if got := get(t, srv.URL, "x.test", "/"); !strings.HasPrefix(got, "B:") {
		t.Errorf("after swap: %q, want B (no restart)", got)
	}
}

func get(t *testing.T, base, host, path string) string {
	t.Helper()
	req, _ := http.NewRequest("GET", base+path, nil)
	req.Host = host
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

// TestIngress_LoadBalancesReplicas guards the keep-alive LB fix: with a dialer
// that round-robins a backend name across replicas, requests must reach EVERY
// replica — not pin to the first one (the keep-alive bug, which sent 100% to a
// single replica). The dialer stands in for the worker's nameTable round-robin.
func TestIngress_LoadBalancesReplicas(t *testing.T) {
	r1 := backend(t, "R1")
	r2 := backend(t, "R2")
	r3 := backend(t, "R3")
	replicas := []string{hostOf(r1), hostOf(r2), hostOf(r3)}

	// Round-robin dialer keyed on the backend NAME "api:80": each dial advances a
	// cursor and connects to the next replica — the in-test analogue of the space
	// nameTable's rotated lookup.
	var mu sync.Mutex
	cursor := 0
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr != "api:80" { // only the named backend is round-robined
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}
		mu.Lock()
		target := replicas[cursor%len(replicas)]
		cursor++
		mu.Unlock()
		return (&net.Dialer{}).DialContext(ctx, network, target)
	}

	h := New().WithDialer(dial)
	h.SetRoutes([]Route{{Host: "app.test", Backend: "api:80"}})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	hits := map[string]int{}
	for i := 0; i < 30; i++ {
		body := get(t, srv.URL, "app.test", "/")
		tag := strings.SplitN(body, ":", 2)[0]
		hits[tag]++
	}

	// All three replicas must have served traffic (the bug: only one would).
	for _, tag := range []string{"R1", "R2", "R3"} {
		if hits[tag] == 0 {
			t.Errorf("replica %s got no traffic — not load-balancing (hits=%v)", tag, hits)
		}
	}
	// And it must be reasonably even (round-robin → 10 each for 30 requests).
	for tag, n := range hits {
		if n < 5 || n > 15 {
			t.Errorf("replica %s got %d/30 — skewed distribution (hits=%v)", tag, n, hits)
		}
	}
}

// A route declaring a timeout must cut a backend that never answers, rather
// than hold the client forever.
func TestHandler_RouteTimeoutCutsSlowBackend(t *testing.T) {
	backendBlocks := make(chan struct{})
	defer close(backendBlocks)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-backendBlocks:
		case <-r.Context().Done():
		}
	}))
	defer backend.Close()

	h := New()
	h.SetRoutes([]Route{{
		Host:    "app.example.com",
		Backend: strings.TrimPrefix(backend.URL, "http://"),
		Timeout: 100 * time.Millisecond,
	}})

	req := httptest.NewRequest(http.MethodGet, "http://app.example.com/", nil)
	req.RemoteAddr = "203.0.113.9:5000"
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		h.ServeHTTP(w, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ServeHTTP never returned — the route timeout did not cut the stalled backend")
	}
	if w.Code == http.StatusOK {
		t.Errorf("stalled backend answered %d, want a proxy error", w.Code)
	}
}

// A route with no timeout must stay unbounded: the default has to keep serving
// large downloads and WebSocket upgrades, which is why the cap is opt-in.
func TestHandler_NoTimeoutLeavesRequestUnbounded(t *testing.T) {
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	h := New()
	h.SetRoutes([]Route{{
		Host:    "app.example.com",
		Backend: strings.TrimPrefix(backend.URL, "http://"),
	}})

	req := httptest.NewRequest(http.MethodGet, "http://app.example.com/", nil)
	req.RemoteAddr = "203.0.113.9:5000"
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		h.ServeHTTP(w, req)
		close(done)
	}()

	// Well past any plausible accidental default.
	select {
	case <-done:
		t.Fatal("request was cut although the route declares no timeout")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	<-done
	if w.Code != http.StatusOK {
		t.Errorf("unbounded route answered %d, want 200", w.Code)
	}
}

// A body over the route's cap must be refused, and the backend must never see
// it — the point is to protect the backend's disk/RAM, not just to report.
func TestHandler_MaxBodyRejectsOversizedRequest(t *testing.T) {
	// The backend handler runs on the test server's own goroutine, so what it
	// records must be guarded: the assertions below read it from the test one.
	var mu sync.Mutex
	var reachedBackend bool
	var readErr error
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.Copy(io.Discard, r.Body)
		mu.Lock()
		reachedBackend, readErr = true, err
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	h := New()
	h.SetRoutes([]Route{{
		Host:    "app.example.com",
		Backend: strings.TrimPrefix(backend.URL, "http://"),
		MaxBody: 1024,
	}})

	body := strings.NewReader(strings.Repeat("x", 64*1024))
	req := httptest.NewRequest(http.MethodPost, "http://app.example.com/upload", body)
	req.RemoteAddr = "203.0.113.11:5000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code == http.StatusOK {
		t.Errorf("oversized body answered 200; want a refusal")
	}
	// Either the proxy never forwarded it, or the backend saw the read cut.
	mu.Lock()
	defer mu.Unlock()
	if reachedBackend && readErr == nil {
		t.Error("backend read the whole oversized body — the cap did not protect it")
	}
}

// A body under the cap must pass through untouched.
func TestHandler_MaxBodyAllowsSmallRequest(t *testing.T) {
	var mu sync.Mutex
	var got int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		mu.Lock()
		got = n
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	h := New()
	h.SetRoutes([]Route{{
		Host:    "app.example.com",
		Backend: strings.TrimPrefix(backend.URL, "http://"),
		MaxBody: 64 * 1024,
	}})

	const size = 4096
	req := httptest.NewRequest(http.MethodPost, "http://app.example.com/upload", strings.NewReader(strings.Repeat("y", size)))
	req.RemoteAddr = "203.0.113.11:5000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("in-budget body answered %d, want 200", w.Code)
	}
	mu.Lock()
	defer mu.Unlock()
	if got != size {
		t.Errorf("backend received %d bytes, want %d", got, size)
	}
}

// No cap declared = unbounded: an upload route must keep working.
func TestHandler_NoMaxBodyIsUnbounded(t *testing.T) {
	var mu sync.Mutex
	var got int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		mu.Lock()
		got = n
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	h := New()
	h.SetRoutes([]Route{{
		Host:    "app.example.com",
		Backend: strings.TrimPrefix(backend.URL, "http://"),
	}})

	const size = 512 * 1024
	req := httptest.NewRequest(http.MethodPost, "http://app.example.com/upload", strings.NewReader(strings.Repeat("z", size)))
	req.RemoteAddr = "203.0.113.11:5000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	mu.Lock()
	defer mu.Unlock()
	if w.Code != http.StatusOK || got != size {
		t.Errorf("uncapped route: code %d, %d bytes through; want 200 and %d", w.Code, got, size)
	}
}
