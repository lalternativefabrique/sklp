package ingress

import (
	"context"
	"net"
	"net/http"
	"strings"
)

// origin is what the handler routes and limits on: the public host, whether
// the client spoke TLS, and the client's IP.
type origin struct {
	host     string
	tls      bool
	clientIP string
}

type relayedKey struct{}

// Relayed wraps h for the listener only the node's Network Controller
// reaches (GatewayService). Requests served through it carry the front's
// X-Forwarded-* headers, and originOf trusts them there. The space worker
// dials every backend from its own gateway address, so the peer address
// cannot tell a relayed connection from a public one; the listener can.
func Relayed(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), relayedKey{}, true)))
	})
}

// originOf reads the origin from the connection, or from the X-Forwarded-*
// headers when the request came through the Relayed listener. Anywhere else
// the headers are client input and ignored.
func originOf(r *http.Request) origin {
	o := origin{host: r.Host, tls: r.TLS != nil, clientIP: peerIP(r)}
	if relayed, _ := r.Context().Value(relayedKey{}).(bool); !relayed {
		return o
	}
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		o.host = h
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		o.tls = strings.EqualFold(p, "https")
	}
	if ff := r.Header.Get("X-Forwarded-For"); ff != "" {
		if first, _, _ := strings.Cut(ff, ","); strings.TrimSpace(first) != "" {
			o.clientIP = strings.TrimSpace(first)
		}
	}
	return o
}

func peerIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
