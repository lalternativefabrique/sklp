package nodeapi

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"sync"
	"time"
)

// DialContext, when set before the first call, opens every connection to a
// node: a caller that reaches nodes by a private route installs it at boot.
var DialContext func(ctx context.Context, network, addr string) (net.Conn, error)

// A transport per request kept every keep-alive connection to a node open
// forever; one per pinned fingerprint reuses them and lets idle ones close.
var transports sync.Map

func clientFor(caFingerprint string) *http.Client {
	fp := normalizeFingerprint(caFingerprint)
	if t, ok := transports.Load(fp); ok {
		return &http.Client{Transport: t.(*http.Transport)}
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if fp != "" {
		tlsCfg.InsecureSkipVerify = true
		tlsCfg.VerifyPeerCertificate = pinnedVerifier(fp)
	}
	t, _ := transports.LoadOrStore(fp, &http.Transport{
		TLSClientConfig:     tlsCfg,
		DialContext:         DialContext,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
	})
	return &http.Client{Transport: t.(*http.Transport)}
}
