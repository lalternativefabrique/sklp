package nodeapi

import (
	"crypto/tls"
	"net/http"
	"sync"
	"time"
)

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
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
	})
	return &http.Client{Transport: t.(*http.Transport)}
}
