package ingress

import (
	"encoding/json"
	"fmt"
	"time"
)

// acme_env.go marshals a resolved ACMEConfig through a single environment
// variable so the spacenet worker (a re-exec of the supervisor binary) inherits
// it without threading config through every constructor layer. The supervisor
// resolves the deploy config + credentials once and exports SKLP_SPACE_ACME; the
// worker decodes it and builds the resolver. The value contains secrets (DNS API
// keys), so it is never logged — it lives only in the worker's environment,
// which is already the trust boundary for the space.

// EnvACME is the environment variable carrying the JSON-encoded ACMEConfig to
// the worker. Empty/absent = the worker serves self-signed (no ACME).
const EnvACME = "SKLP_SPACE_ACME"

// acmeEnvJSON is the wire shape in EnvACME. PropagationTimeout is seconds (a
// duration does not JSON-round-trip cleanly across the process boundary).
type acmeEnvJSON struct {
	Email              string            `json:"email"`
	CADirURL           string            `json:"ca,omitempty"`
	Staging            bool              `json:"staging,omitempty"`
	StorageDir         string            `json:"storage_dir"`
	DNSProvider        string            `json:"dns_provider"`
	DNSCredentials     map[string]string `json:"dns_credentials"`
	PropagationSeconds int               `json:"propagation_seconds,omitempty"`
}

// MarshalACMEEnv encodes cfg for EnvACME. The caller sets it on the worker's
// environment. Returns "" for a zero config so an absent ACME setup yields no
// env var at all.
func MarshalACMEEnv(cfg ACMEConfig) (string, error) {
	if cfg.Email == "" && cfg.DNSProvider == "" {
		return "", nil
	}
	b, err := json.Marshal(acmeEnvJSON{
		Email:              cfg.Email,
		CADirURL:           cfg.CADirURL,
		Staging:            cfg.Staging,
		StorageDir:         cfg.StorageDir,
		DNSProvider:        cfg.DNSProvider,
		DNSCredentials:     cfg.DNSCredentials,
		PropagationSeconds: int(cfg.PropagationTimeout / time.Second),
	})
	if err != nil {
		return "", fmt.Errorf("acme: marshal env: %w", err)
	}
	return string(b), nil
}

// UnmarshalACMEEnv decodes an EnvACME value back into an ACMEConfig. An empty
// value returns ok=false (no ACME configured), not an error.
func UnmarshalACMEEnv(v string) (ACMEConfig, bool, error) {
	if v == "" {
		return ACMEConfig{}, false, nil
	}
	var j acmeEnvJSON
	if err := json.Unmarshal([]byte(v), &j); err != nil {
		return ACMEConfig{}, false, fmt.Errorf("acme: unmarshal env: %w", err)
	}
	return ACMEConfig{
		Email:              j.Email,
		CADirURL:           j.CADirURL,
		Staging:            j.Staging,
		StorageDir:         j.StorageDir,
		DNSProvider:        j.DNSProvider,
		DNSCredentials:     j.DNSCredentials,
		PropagationTimeout: time.Duration(j.PropagationSeconds) * time.Second,
	}, true, nil
}
