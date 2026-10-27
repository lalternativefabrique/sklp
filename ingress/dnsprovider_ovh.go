package ingress

import (
	"github.com/caddyserver/certmagic"
	"github.com/libdns/ovh"
)

// dnsprovider_ovh.go registers the OVH dns-01 provider. OVH's API uses an
// application-triple auth (app key/secret + a consumer key scoped to the zones).
// Credentials come from the env names declared in gateway.acme.credentials and
// are resolved by the caller into this map — the secret never touches YAML.
//
// Default env names (overridable in config): OVH_ENDPOINT, OVH_APPLICATION_KEY,
// OVH_APPLICATION_SECRET, OVH_CONSUMER_KEY. Endpoint defaults to ovh-eu.

func init() {
	RegisterDNSProvider("ovh", newOVHProvider)
}

func newOVHProvider(creds map[string]string) (certmagic.DNSProvider, error) {
	appKey, err := reqCred(creds, "OVH_APPLICATION_KEY")
	if err != nil {
		return nil, err
	}
	appSecret, err := reqCred(creds, "OVH_APPLICATION_SECRET")
	if err != nil {
		return nil, err
	}
	consumerKey, err := reqCred(creds, "OVH_CONSUMER_KEY")
	if err != nil {
		return nil, err
	}
	endpoint := creds["OVH_ENDPOINT"]
	if endpoint == "" {
		endpoint = "ovh-eu"
	}
	return &ovh.Provider{
		Endpoint:          endpoint,
		ApplicationKey:    appKey,
		ApplicationSecret: appSecret,
		ConsumerKey:       consumerKey,
	}, nil
}
