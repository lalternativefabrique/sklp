package ingress

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func forwarded(t *testing.T, h http.Handler, remote string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://sklp-l7.alpha.internal/api/x", nil)
	req.RemoteAddr = remote
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestForwarded_RelayedListenerRoutesOnForwardedHost(t *testing.T) {
	api := backend(t, "API")
	h := New()
	h.SetRoutes([]Route{{Host: "app.test", Path: "/api", Backend: hostOf(api)}})

	w := forwarded(t, Relayed(h), "10.88.0.1:40000", map[string]string{"X-Forwarded-Host": "app.test"})
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Body.String(), "API:") {
		t.Fatalf("relayed request: %d %q, want API backend", w.Code, w.Body.String())
	}
}

func TestForwarded_PlainListenerIgnoresForwardedHost(t *testing.T) {
	api := backend(t, "API")
	h := New()
	h.SetRoutes([]Route{{Host: "app.test", Path: "/api", Backend: hostOf(api)}})

	w := forwarded(t, h, "10.88.0.1:40000", map[string]string{"X-Forwarded-Host": "app.test"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("client-supplied X-Forwarded-Host was honoured: %d", w.Code)
	}
}

func TestForwarded_RateLimitsOnForwardedClientIP(t *testing.T) {
	api := backend(t, "API")
	h := New()
	h.SetRoutes([]Route{{Host: "app.test", Path: "/api", Backend: hostOf(api), Rate: RateSpec{RPS: 1, Burst: 1}}})

	hit := func(ip string) int {
		return forwarded(t, Relayed(h), "10.88.0.1:40000", map[string]string{
			"X-Forwarded-Host": "app.test",
			"X-Forwarded-For":  ip + ", 10.90.0.1",
		}).Code
	}
	if c := hit("198.51.100.1"); c != http.StatusOK {
		t.Fatalf("first request from client 1: %d", c)
	}
	if c := hit("198.51.100.1"); c != http.StatusTooManyRequests {
		t.Fatalf("second request from client 1 not limited: %d", c)
	}
	if c := hit("198.51.100.2"); c != http.StatusOK {
		t.Fatalf("client 2 shares client 1's bucket: %d", c)
	}
}

func TestForwarded_ProtoDrivesHSTSAndApexRedirect(t *testing.T) {
	api := backend(t, "API")
	h := New()
	h.SetRoutes([]Route{{Host: "app.test", Backend: hostOf(api), SecurityHeaders: true, RedirectApex: true}})

	w := forwarded(t, Relayed(h), "10.88.0.1:40000", map[string]string{"X-Forwarded-Host": "app.test", "X-Forwarded-Proto": "https"})
	if w.Header().Get("Strict-Transport-Security") == "" {
		t.Fatal("HSTS missing on a request the front terminated as https")
	}
	w = forwarded(t, Relayed(h), "10.88.0.1:40000", map[string]string{"X-Forwarded-Host": "www.app.test", "X-Forwarded-Proto": "https"})
	if w.Code != http.StatusMovedPermanently || !strings.HasPrefix(w.Header().Get("Location"), "https://app.test/") {
		t.Fatalf("www redirect: %d %q", w.Code, w.Header().Get("Location"))
	}
}
