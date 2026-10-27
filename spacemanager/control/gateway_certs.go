package control

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"
)

// certDialTimeout caps one handshake. The front door is local, so a host that
// has not answered by then is down rather than slow.
const certDialTimeout = 5 * time.Second

// certProbeParallelism bounds the concurrent handshakes: a node fronting dozens
// of hosts would otherwise open them all at once against its own gateway.
const certProbeParallelism = 8

// serveGatewayCerts answers GET RouteGatewayCerts: the certificate each routed
// host is actually served.
//
// It dials the front door and reads the certificate off the handshake, rather
// than parsing the driver's store. That is deliberate: the handshake is what a
// visitor gets, it answers the same way whichever front door is installed
// (traefik's acme.json and the native L7's certmagic keep different formats),
// and it never opens a file holding private keys.
func serveGatewayCerts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	status, _ := gatewayHooks()
	if status == nil {
		http.Error(w, "no gateway is opened on this node", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(GatewayCertsResponse{Certs: probeCerts(routedHosts())})
}

// routedHosts is every host this node routes, deduplicated and sorted. Several
// routes of several spaces share one host, and one certificate covers them all.
func routedHosts() []string {
	spaces, err := ListSpaces()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	for _, space := range spaces {
		_, meta, err := ReadPersistedManifest(space)
		if err != nil {
			continue
		}
		for _, route := range meta.Routes {
			if route.Host != "" {
				seen[route.Host] = true
			}
		}
	}
	hosts := make([]string, 0, len(seen))
	for host := range seen {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	return hosts
}

func probeCerts(hosts []string) []GatewayCert {
	certs := make([]GatewayCert, len(hosts))
	sem := make(chan struct{}, certProbeParallelism)
	var wg sync.WaitGroup
	for i, host := range hosts {
		wg.Add(1)
		go func(i int, host string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			certs[i] = probeCert(host)
		}(i, host)
	}
	wg.Wait()
	return certs
}

// probeCert reads the certificate one host is served. Verification is disabled
// on purpose: a self-signed fallback is exactly the state worth reporting, and
// failing the handshake over it would hide the answer.
func probeCert(host string) GatewayCert { return probeCertAt(host, "443") }

func probeCertAt(host, port string) GatewayCert {
	out := GatewayCert{Host: host}
	conn, err := tls.DialWithDialer(
		&net.Dialer{Timeout: certDialTimeout},
		"tcp",
		net.JoinHostPort(host, port),
		&tls.Config{ServerName: host, InsecureSkipVerify: true},
	)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	defer conn.Close()

	chain := conn.ConnectionState().PeerCertificates
	if len(chain) == 0 {
		out.Error = "the host served no certificate"
		return out
	}
	leaf := chain[0]
	out.NotAfter = leaf.NotAfter
	out.Issuer = leaf.Issuer.CommonName
	out.SelfSigned = isSelfSigned(leaf)
	if out.SelfSigned {
		out.Issuer = ""
	}
	return out
}

// isSelfSigned reports whether the leaf signed itself — the shape of the
// fallback certificate a front door serves when ACME never issued for a host.
//
// The check is the signature over the raw names, not the CommonNames: the
// fallback cert carries an Organization and no CommonName, so comparing CNs
// would match two empty strings and call a CA-issued leaf self-signed too.
// CheckSignatureFrom is not usable here — it additionally requires the signer
// to be a CA, which a self-signed leaf is not.
func isSelfSigned(leaf *x509.Certificate) bool {
	if !bytes.Equal(leaf.RawIssuer, leaf.RawSubject) {
		return false
	}
	return leaf.CheckSignature(leaf.SignatureAlgorithm, leaf.RawTBSCertificate, leaf.Signature) == nil
}
