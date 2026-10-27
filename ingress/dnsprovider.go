package ingress

import (
	"fmt"
	"sort"
	"sync"

	"github.com/caddyserver/certmagic"
)

// dnsprovider.go is the provider-agnostic seam for the ACME dns-01 challenge.
// certmagic solves dns-01 through a libdns provider (any type implementing the
// libdns record interfaces). Rather than hard-wire one, we register providers by
// name and build the requested one from a credential map, so a deploy selects
// its DNS API with `gateway.acme.dns_provider: ovh` and the matching provider is
// constructed from env-sourced credentials. New providers (scaleway,
// cloudflare, ...) register the same way — one file, no changes here.

// DNSProviderFactory builds a certmagic-compatible dns-01 solver backend from a
// credential map (the resolved values of the env names declared in the deploy
// config). It returns the libdns provider as certmagic.DNSProvider; a missing or
// malformed credential is a config error surfaced at gateway start, not a silent
// self-signed fallback.
type DNSProviderFactory func(creds map[string]string) (certmagic.DNSProvider, error)

var (
	dnsMu        sync.RWMutex
	dnsProviders = map[string]DNSProviderFactory{}
)

// RegisterDNSProvider adds a named dns-01 provider factory. Called from each
// provider file's init(). A duplicate name panics — a programming error, caught
// at startup.
func RegisterDNSProvider(name string, f DNSProviderFactory) {
	dnsMu.Lock()
	defer dnsMu.Unlock()
	if _, dup := dnsProviders[name]; dup {
		panic(fmt.Sprintf("ingress: duplicate DNS provider %q", name))
	}
	dnsProviders[name] = f
}

// NewDNSProvider builds the named provider from creds. Unknown name lists the
// registered providers so a typo in `dns_provider:` is diagnosable.
func NewDNSProvider(name string, creds map[string]string) (certmagic.DNSProvider, error) {
	dnsMu.RLock()
	f, ok := dnsProviders[name]
	dnsMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("ingress: unknown DNS provider %q (have: %v)", name, KnownDNSProviders())
	}
	return f(creds)
}

// KnownDNSProviders lists registered provider names, sorted, for validation and
// error messages.
func KnownDNSProviders() []string {
	dnsMu.RLock()
	defer dnsMu.RUnlock()
	names := make([]string, 0, len(dnsProviders))
	for n := range dnsProviders {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// KnownDNSProvider reports whether name is registered (parse-time validation).
func KnownDNSProvider(name string) bool {
	dnsMu.RLock()
	defer dnsMu.RUnlock()
	_, ok := dnsProviders[name]
	return ok
}

// reqCred fetches a required credential, erroring with the key name so a missing
// env is actionable ("OVH_APPLICATION_KEY not set"). Empty counts as missing.
func reqCred(creds map[string]string, key string) (string, error) {
	v := creds[key]
	if v == "" {
		return "", fmt.Errorf("ingress: DNS provider credential %q is empty", key)
	}
	return v, nil
}
