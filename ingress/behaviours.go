package ingress

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"sync"
)

// hstsMaxAge is one year in seconds, the value the HSTS preload list requires.
// It is a one-way door for a domain: browsers refuse plain http for that long,
// so the header is only ever sent when a route asked for it.
const hstsMaxAge = "max-age=31536000; includeSubDomains"

// compressibleTypes are the content types worth gzipping. Anything already
// compressed (images, video, archives) is left alone: re-compressing costs CPU
// and grows the payload.
var compressibleTypes = []string{
	"text/html", "text/css", "text/plain", "text/xml",
	"application/javascript", "application/json", "image/svg+xml",
}

// compressMinSize is the payload floor below which gzip is not worth its own
// header overhead.
const compressMinSize = 1024

// apexOf returns the bare host of a "www." host, and whether it had the prefix.
func apexOf(host string) (string, bool) {
	if strings.HasPrefix(host, "www.") {
		return strings.TrimPrefix(host, "www."), true
	}
	return host, false
}

// redirectToApex answers a www request with a permanent redirect to the same
// URL on the bare host.
func redirectToApex(w http.ResponseWriter, r *http.Request, apex string, isTLS bool) {
	scheme := "http"
	if isTLS {
		scheme = "https"
	}
	target := scheme + "://" + apex + r.URL.RequestURI()
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}

func setSecurityHeaders(h http.Header, isTLS bool) {
	// HSTS over plain http is meaningless and the RFC says not to send it.
	if isTLS {
		h.Set("Strict-Transport-Security", hstsMaxAge)
	}
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
}

var gzipWriters = sync.Pool{
	New: func() any { return gzip.NewWriter(io.Discard) },
}

// gzipResponseWriter compresses the body when the response turns out to be a
// compressible type above the size floor.
//
// The decision is taken in WriteHeader, not on the first Write: the reverse
// proxy copies the backend's headers and calls WriteHeader before any body
// byte, so deciding later would mean rewriting headers the client already has.
// A response the backend streams without WriteHeader is caught by the Write
// path, which decides first.
type gzipResponseWriter struct {
	http.ResponseWriter
	gz       *gzip.Writer
	decided  bool
	compress bool
}

func (w *gzipResponseWriter) WriteHeader(status int) {
	if !w.decided {
		w.decide()
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *gzipResponseWriter) Write(p []byte) (int, error) {
	if !w.decided {
		w.decide()
	}
	if w.compress {
		return w.gz.Write(p)
	}
	return w.ResponseWriter.Write(p)
}

// decide inspects the response headers and either starts gzipping or gets out
// of the way for good. Called before the headers reach the client, so it may
// still rewrite them.
func (w *gzipResponseWriter) decide() {
	w.decided = true
	h := w.Header()
	if h.Get("Content-Encoding") != "" {
		return
	}
	if !isCompressible(h.Get("Content-Type")) {
		return
	}
	// A declared length below the floor is not worth its own gzip header. An
	// absent length means a streamed response, which is compressed: its size is
	// unknown, and streaming is exactly the case where compression pays.
	if cl := h.Get("Content-Length"); cl != "" {
		if n, err := parseLen(cl); err == nil && n < compressMinSize {
			return
		}
	}

	h.Del("Content-Length") // the compressed length is not known up front
	h.Set("Content-Encoding", "gzip")
	h.Add("Vary", "Accept-Encoding")

	gz := gzipWriters.Get().(*gzip.Writer)
	gz.Reset(w.ResponseWriter)
	w.gz, w.compress = gz, true
}

// Close flushes and releases the gzip writer. Safe on a response that was never
// compressed.
func (w *gzipResponseWriter) Close() {
	if w.compress && w.gz != nil {
		_ = w.gz.Close()
		gzipWriters.Put(w.gz)
		w.gz = nil
	}
}

// Flush forwards a backend flush to the client. The reverse proxy flushes
// after every write of a text/event-stream and after each chunk of any
// streamed body; without this method the wrapper hides the underlying Flusher
// and the bytes sit in net/http's buffer until the response ends, which a
// long-lived SSE never does.
func (w *gzipResponseWriter) Flush() {
	if !w.decided {
		w.decide()
	}
	if w.compress && w.gz != nil {
		_ = w.gz.Flush()
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the wrapped writer to http.ResponseController, so hijacks
// and flushes reach the connection even through this wrapper.
func (w *gzipResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func isCompressible(contentType string) bool {
	if i := strings.IndexByte(contentType, ';'); i >= 0 {
		contentType = contentType[:i]
	}
	contentType = strings.TrimSpace(strings.ToLower(contentType))
	for _, t := range compressibleTypes {
		if contentType == t {
			return true
		}
	}
	return false
}

func parseLen(s string) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errNotANumber
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

var errNotANumber = &lenError{}

type lenError struct{}

func (*lenError) Error() string { return "not a number" }

func acceptsGzip(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
}
