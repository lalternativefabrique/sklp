package ingress

import (
	"bufio"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// typedBackend serves a fixed body under a chosen content type, so the
// compression decision (which reads Content-Type) can be exercised.
func typedBackend(t *testing.T, body, contentType string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		io.WriteString(w, body)
	}))
	t.Cleanup(s.Close)
	return s
}

// do issues a request through the ingress and returns the whole response.
func do(t *testing.T, base, host, path string, headers map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", base+path, nil)
	req.Host = host
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	// The redirect must be observed, not followed.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func serve(t *testing.T, routes ...Route) string {
	t.Helper()
	h := New()
	h.SetRoutes(routes)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestApexRedirectAnswersWWW: "www.<host>" matches no route of its own, since
// redirect_apex is declared on the apex route. The handler must resolve it
// against that route rather than 404.
func TestApexRedirectAnswersWWW(t *testing.T) {
	b := backend(t, "WEB")
	base := serve(t, Route{Host: "example.com", Backend: hostOf(b), RedirectApex: true})

	resp := do(t, base, "www.example.com", "/a/b?q=1", nil)
	if resp.StatusCode != http.StatusMovedPermanently {
		t.Fatalf("status = %d, want 301", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "http://example.com/a/b?q=1" {
		t.Fatalf("Location = %q, want the path and query preserved", got)
	}
}

// TestApexRedirectOffStill404s: without the flag a www host is simply unrouted,
// so the behaviour of every existing deploy is unchanged.
func TestApexRedirectOffStill404s(t *testing.T) {
	b := backend(t, "WEB")
	base := serve(t, Route{Host: "example.com", Backend: hostOf(b)})

	if resp := do(t, base, "www.example.com", "/", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when redirect_apex is off", resp.StatusCode)
	}
}

func TestSecurityHeadersAreSet(t *testing.T) {
	b := backend(t, "WEB")
	base := serve(t, Route{Host: "example.com", Backend: hostOf(b), SecurityHeaders: true})

	resp := do(t, base, "example.com", "/", nil)
	for header, want := range map[string]string{
		"X-Frame-Options":        "DENY",
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
	} {
		if got := resp.Header.Get(header); got != want {
			t.Fatalf("%s = %q, want %q", header, got, want)
		}
	}
}

// TestHSTSOnlyOverTLS: HSTS is a one-way door for a domain and means nothing on
// plain http, so it is never sent on a non-TLS request.
func TestHSTSOnlyOverTLS(t *testing.T) {
	b := backend(t, "WEB")
	base := serve(t, Route{Host: "example.com", Backend: hostOf(b), SecurityHeaders: true})

	resp := do(t, base, "example.com", "/", nil)
	if got := resp.Header.Get("Strict-Transport-Security"); got != "" {
		t.Fatalf("HSTS = %q, want it absent over plain http", got)
	}
}

func TestCompressGzipsLargeHTML(t *testing.T) {
	body := strings.Repeat("<p>hello</p>", 200)
	b := typedBackend(t, body, "text/html; charset=utf-8")
	base := serve(t, Route{Host: "example.com", Backend: hostOf(b), Compress: true})

	resp := do(t, base, "example.com", "/", map[string]string{"Accept-Encoding": "gzip"})
	if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("the body is not valid gzip: %v", err)
	}
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read gzip: %v", err)
	}
	if string(got) != body {
		t.Fatal("the decompressed body does not match what the backend sent")
	}
}

// TestCompressSkippedWithoutAcceptEncoding: compressing for a client that did
// not ask hands it a body it cannot read.
func TestCompressSkippedWithoutAcceptEncoding(t *testing.T) {
	body := strings.Repeat("<p>hello</p>", 200)
	b := typedBackend(t, body, "text/html")
	base := serve(t, Route{Host: "example.com", Backend: hostOf(b), Compress: true})

	resp := do(t, base, "example.com", "/", map[string]string{"Accept-Encoding": "identity"})
	if resp.Header.Get("Content-Encoding") == "gzip" {
		t.Fatal("compressed a response the client did not ask to be compressed")
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != body {
		t.Fatal("the body was altered")
	}
}

// TestCompressSkipsSmallAndBinary: gzip costs CPU and grows already-compressed
// payloads, so it applies to text above a size floor only.
func TestCompressSkipsSmallAndBinary(t *testing.T) {
	for _, tc := range []struct{ name, body, ctype string }{
		{"small", "tiny", "text/html"},
		{"binary", strings.Repeat("x", 4096), "image/png"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := typedBackend(t, tc.body, tc.ctype)
			base := serve(t, Route{Host: "example.com", Backend: hostOf(b), Compress: true})

			resp := do(t, base, "example.com", "/", map[string]string{"Accept-Encoding": "gzip"})
			if resp.Header.Get("Content-Encoding") == "gzip" {
				t.Fatalf("%s should not be compressed", tc.name)
			}
		})
	}
}

// TestNoBehavioursLeavesResponseUntouched: a plain route must behave exactly as
// it did before these fields existed.
func TestNoBehavioursLeavesResponseUntouched(t *testing.T) {
	body := strings.Repeat("<p>hello</p>", 200)
	b := typedBackend(t, body, "text/html")
	base := serve(t, Route{Host: "example.com", Backend: hostOf(b)})

	resp := do(t, base, "example.com", "/", map[string]string{"Accept-Encoding": "gzip"})
	if resp.Header.Get("Content-Encoding") != "" {
		t.Fatal("a plain route compressed its response")
	}
	if resp.Header.Get("X-Frame-Options") != "" {
		t.Fatal("a plain route set security headers")
	}
}

// TestCompressStreamedResponse: a backend that flushes without declaring a
// length is compressed. Its size is unknown at header time, and streaming is
// the case where compression pays most — so an absent Content-Length must not
// read as "too small".
func TestCompressStreamedResponse(t *testing.T) {
	chunk := strings.Repeat("<p>hello</p>", 200)
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		io.WriteString(w, chunk)
	}))
	t.Cleanup(b.Close)

	base := serve(t, Route{Host: "example.com", Backend: hostOf(b), Compress: true})
	resp := do(t, base, "example.com", "/", map[string]string{"Accept-Encoding": "gzip"})

	if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip on a streamed response", got)
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("the body is not valid gzip: %v", err)
	}
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read gzip: %v", err)
	}
	if string(got) != chunk {
		t.Fatal("the decompressed body does not match what the backend streamed")
	}
}

// TestCompressRouteStreamsSSE: a compress route must deliver each flushed
// event of a text/event-stream to the client while the backend keeps the
// response open. The gzip wrapper is installed before the content type is
// known, so it has to relay Flush even when it ends up not compressing.
func TestCompressRouteStreamsSSE(t *testing.T) {
	release := make(chan struct{})
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(b.Close)
	t.Cleanup(func() { close(release) })

	base := serve(t, Route{Host: "example.com", Backend: hostOf(b), Compress: true})

	type first struct {
		encoding string
		line     string
		err      error
	}
	got := make(chan first, 1)
	go func() {
		resp := do(t, base, "example.com", "/events", map[string]string{"Accept-Encoding": "gzip"})
		defer resp.Body.Close()
		line, err := bufio.NewReader(resp.Body).ReadString('\n')
		got <- first{resp.Header.Get("Content-Encoding"), line, err}
	}()
	select {
	case r := <-got:
		if r.encoding != "" {
			t.Fatalf("Content-Encoding = %q, an event stream must not be gzipped", r.encoding)
		}
		if r.err != nil || r.line != "data: first\n" {
			t.Fatalf("first event = %q, %v", r.line, r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the flushed event did not reach the client while the stream was open")
	}
}
