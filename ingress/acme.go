package ingress

import (
	"context"
	"crypto/tls"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/certmagic"
)

// acme.go is the real-cert source for the native L7 TLS listener. It wraps
// certmagic to obtain Let's Encrypt (or any ACME CA) certificates via the
// dns-01 challenge — the only challenge that works behind sklp's SNI-passthrough
// mux (the mux routes :9443 by SNI to the owning space, so it can neither answer
// TLS-ALPN-01 nor serve HTTP-01 before the domain has a route; dns-01 needs
// nothing but the DNS API, and it is the only challenge that issues wildcards).
//
// It is a drop-in for SelfSignedResolver at the tls.Config.GetCertificate seam:
// managed public domains get an ACME cert; everything else (*.test, localhost,
// an unmanaged host) falls back to a per-SNI self-signed cert, so dev and
// bare-hostname routes keep working with zero config. Certs and the ACME account
// persist on disk (FileStorage) so a worker restart never re-hits the CA — the
// equivalent of traefik's acme.json.

// ACMEConfig is the resolved (secrets-included) ACME configuration handed to the
// L7 listener. It is built host-side from the deploy config + env, then threaded
// to the worker; the worker never parses YAML.
type ACMEConfig struct {
	// Email is the ACME account contact (Let's Encrypt rejects the placeholder
	// admin@example.com). Reuses gateway.email.
	Email string

	// CADirURL is the ACME directory endpoint. Empty = Let's Encrypt production.
	// Point at the staging directory for staging→prod promotion without hitting
	// the strict prod rate limits.
	CADirURL string

	// Staging, when true, selects the Let's Encrypt STAGING CA regardless of
	// CADirURL (a convenience over spelling the URL). Staging certs are untrusted
	// but exercise the full issuance path.
	Staging bool

	// StorageDir is where certmagic persists the account key, certs, and locks.
	// REQUIRED for real ACME (otherwise every restart re-issues and burns rate
	// limit). A per-node path shared across spaces is fine — keys are namespaced.
	StorageDir string

	// DNSProvider is the registered provider name (e.g. "ovh") for the dns-01
	// solver.
	DNSProvider string

	// DNSCredentials is the resolved credential map (env name -> value) for the
	// provider. Never logged.
	DNSCredentials map[string]string

	// PropagationTimeout bounds how long to wait for the challenge TXT record to
	// propagate before asking the CA to validate. 0 = certmagic default (2m).
	PropagationTimeout time.Duration

	// SpaceDNSAddr is the "ip:port" of the space DNS resolver (the worker's own
	// forwarder on the bridge gateway, e.g. "10.88.0.1:53"). REQUIRED for
	// issuance from inside the space netns: the worker's default Go resolver
	// reads the HOST /etc/resolv.conf, whose nameserver (127.0.0.53 on a
	// systemd-resolved host) is unreachable across the netns boundary — so
	// resolving acme-v02.api.letsencrypt.org / the DNS provider API would fail
	// silently and every issuance would hang. Pointing certmagic at the space DNS
	// (which forwards external names upstream) fixes it. Empty = the default Go
	// resolver (correct only when the host resolver is reachable in-netns).
	SpaceDNSAddr string
}

// LetsEncryptStagingCA is certmagic's Let's Encrypt staging directory constant.
var LetsEncryptStagingCA = certmagic.LetsEncryptStagingCA

// ACMEResolver is a tls.Config.GetCertificate source that serves ACME certs for
// managed public domains and self-signed certs for everything else. It owns a
// certmagic Config + Cache and the self-signed fallback. Safe for concurrent
// use. Build with NewACMEResolver; call ManageDomains as the space's public
// hostnames change; GetCertificate is the tls seam; Stop releases the cache.
type ACMEResolver struct {
	magic     *certmagic.Config
	cache     *certmagic.Cache
	fallback  *SelfSignedResolver
	issuerKey string // certmagic issuer key, to evict unmanaged certs from the cache

	// manageAsync issues certs for new domains. Defaults to magic.ManageAsync;
	// injectable so tests exercise the managed-set reconciliation without a CA
	// round-trip.
	manageAsync func(context.Context, []string) error

	mu      sync.Mutex
	managed map[string]bool // lower-case public host currently under ACME management
}

// NewACMEResolver builds the certmagic pipeline from cfg: a FileStorage-backed
// Config with a dns-01 issuer using the named provider. It does not contact the
// CA — certificates are obtained lazily on first ManageDomains / handshake. A
// bad provider name or missing credential errors here, so a misconfigured ACME
// block fails the gateway start loudly instead of silently self-signing.
func NewACMEResolver(cfg ACMEConfig) (*ACMEResolver, error) {
	if cfg.Email == "" {
		return nil, fmt.Errorf("acme: email is required (set gateway.email)")
	}
	if cfg.StorageDir == "" {
		return nil, fmt.Errorf("acme: storage dir is required")
	}
	provider, err := NewDNSProvider(cfg.DNSProvider, cfg.DNSCredentials)
	if err != nil {
		return nil, err
	}

	ca := cfg.CADirURL
	if cfg.Staging {
		ca = certmagic.LetsEncryptStagingCA
	}

	// Build the solver in place — DNSManager embeds a sync.Mutex, so it must not
	// be copied by value after construction.
	solver := &certmagic.DNS01Solver{
		DNSManager: certmagic.DNSManager{
			DNSProvider:        provider,
			PropagationTimeout: cfg.PropagationTimeout,
		},
	}
	if cfg.SpaceDNSAddr != "" {
		// Propagation checks must query through the space DNS too, not the host
		// resolver that is unreachable in-netns.
		solver.DNSManager.Resolvers = []string{cfg.SpaceDNSAddr}
	}
	fallback := NewSelfSignedResolver()

	tmpl := certmagic.Config{
		Storage: &certmagic.FileStorage{Path: cfg.StorageDir},
	}
	// The cache asks GetConfigForCert for the Config to manage each cert with. We
	// return one Config bound to THIS cache (captured below) so renewals reuse the
	// same storage/account — not a fresh cache per cert (which would leak
	// maintenance goroutines).
	var cache *certmagic.Cache
	cache = certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(certmagic.Certificate) (*certmagic.Config, error) {
			return certmagic.New(cache, tmpl), nil
		},
	})
	magic := certmagic.New(cache, tmpl)
	// NewACMEIssuer requires an associated Config, so it is built AFTER magic and
	// assigned back onto it (the certmagic-documented order: New → NewACMEIssuer
	// → assign to Issuers).
	issuer := certmagic.NewACMEIssuer(magic, certmagic.ACMEIssuer{
		CA:                      ca,
		Email:                   cfg.Email,
		Agreed:                  true,
		DNS01Solver:             solver,
		Resolver:                cfg.SpaceDNSAddr, // reach the CA via the space DNS, not the host resolver
		DisableHTTPChallenge:    true,
		DisableTLSALPNChallenge: true,
	})
	magic.Issuers = []certmagic.Issuer{issuer}

	r := &ACMEResolver{
		magic:     magic,
		cache:     cache,
		fallback:  fallback,
		issuerKey: issuer.IssuerKey(),
		managed:   map[string]bool{},
	}
	r.manageAsync = r.magic.ManageAsync
	return r, nil
}

// normHost lower-cases a host so route hosts (which may be mixed-case) and the
// SNI a browser sends (always lower-case) compare equal. Applied on both the
// management and lookup sides.
func normHost(h string) string { return strings.ToLower(strings.TrimSpace(h)) }

// ManageDomains reconciles the set of public domains under ACME management to
// exactly hosts (the space's current public route hosts, including any wildcard
// like *.apps.example.com). New domains trigger async issuance (background — the
// handshake serves self-signed until the cert lands, then upgrades); it never
// blocks a deploy. Renewal is handled by certmagic's cache maintenance loop.
// Idempotent: calling with the same set is a cheap no-op.
func (r *ACMEResolver) ManageDomains(ctx context.Context, hosts []string) error {
	want := make(map[string]bool, len(hosts))
	var toManage []string
	var toEvict []certmagic.SubjectIssuer
	r.mu.Lock()
	for _, h := range hosts {
		h = normHost(h)
		if h == "" {
			continue
		}
		want[h] = true
		if !r.managed[h] {
			toManage = append(toManage, h)
		}
	}
	// Domains no longer routed are actively unmanaged: dropped from the managed
	// set (so GetCertificate falls back to self-signed) AND evicted from the
	// cache, so certmagic's maintenance loop stops renewing them (otherwise it
	// keeps hitting the CA + DNS API for a domain that is no longer routed).
	for h := range r.managed {
		if !want[h] {
			toEvict = append(toEvict, certmagic.SubjectIssuer{Subject: h, IssuerKey: r.issuerKey})
		}
	}
	r.managed = want
	r.mu.Unlock()

	if len(toEvict) > 0 && r.cache != nil {
		r.cache.RemoveManaged(toEvict)
	}
	if len(toManage) == 0 {
		return nil
	}
	// Async: returns before the CA round-trip. Errors here are pre-flight only.
	return r.manageAsync(ctx, toManage)
}

// GetCertificate is the tls.Config.GetCertificate callback. For a managed public
// SNI it returns the ACME cert from certmagic (self-signed while issuance is
// still in flight); for everything else it returns a per-SNI self-signed cert.
// A client with no SNI gets the self-signed no-SAN cert.
func (r *ACMEResolver) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if r.isManaged(hello.ServerName) {
		cert, err := r.magic.GetCertificate(hello)
		if err == nil {
			return cert, nil
		}
		// Issuance not finished yet (or transient) — serve self-signed so the
		// handshake still completes; the client re-connects once the cert lands.
	}
	return r.fallback.GetCertificate(hello)
}

// isManaged reports whether host is under ACME management, matching a wildcard
// managed domain (*.example.com) against a concrete SNI (app.example.com).
func (r *ACMEResolver) isManaged(host string) bool {
	host = normHost(host)
	if host == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.managed[host] {
		return true
	}
	// Wildcard match: a managed "*.example.com" covers "app.example.com".
	for m := range r.managed {
		if len(m) > 2 && m[0] == '*' && m[1] == '.' {
			if suffix := m[1:]; hasDotSuffix(host, suffix) {
				return true
			}
		}
	}
	return false
}

// hasDotSuffix reports whether host is a single-label child of suffix
// (".example.com"): "app.example.com" matches, "example.com" and
// "a.b.example.com" do not — wildcards cover exactly one label, per RFC 6125.
func hasDotSuffix(host, suffix string) bool {
	if len(host) <= len(suffix) || host[len(host)-len(suffix):] != suffix {
		return false
	}
	label := host[:len(host)-len(suffix)]
	for i := 0; i < len(label); i++ {
		if label[i] == '.' {
			return false
		}
	}
	return label != ""
}

// IsManagedForTest reports whether host is under ACME management. Exported for
// cross-package tests (the native L7 verifies its domain reconciliation).
func (r *ACMEResolver) IsManagedForTest(host string) bool {
	return r.isManaged(host)
}

// DisableIssuanceForTest replaces the async issuer with a no-op so cross-package
// tests exercise domain reconciliation without a CA round-trip.
func (r *ACMEResolver) DisableIssuanceForTest() {
	r.manageAsync = func(context.Context, []string) error { return nil }
}

// Stop releases the certmagic cache maintenance goroutine. Call on worker
// shutdown.
func (r *ACMEResolver) Stop() {
	if r.cache != nil {
		r.cache.Stop()
	}
}
