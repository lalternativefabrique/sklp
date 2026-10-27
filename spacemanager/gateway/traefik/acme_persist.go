package traefik

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/lalternativefabrique/sklp/ingress"
)

// acme_persist.go keeps the resolved gateway.acme settings on disk beside
// traefik.yaml, so relaunching the gateway does not silently downgrade it.
//
// The settings reach the driver through SKLP_SPACE_ACME, which spacemanager
// sets in its own process while reading deploy.yaml. Any start that does not
// come from a deploy — `sklp gateway up`, the supervision watchdog, a reboot —
// has no such variable, so resolvedACME returned empty and the static config
// was rewritten with httpChallenge. That challenge cannot issue a wildcard, and
// the repeated failures burned Let's Encrypt authorization rate limits on
// unrelated hosts.
//
// Credentials are deliberately NOT persisted: the file records the provider and
// the NAMES of the env vars holding the DNS API keys, matching deploy.yaml's own
// rule that credentials name env vars and never carry values. On a relaunch the
// values are read back from the process environment.

const acmeConfigFile = "acme-config.json"

// persistedACME is the on-disk shape. It mirrors the env wire format minus the
// credential values, which are replaced by the env var names to read them from.
type persistedACME struct {
	Email              string   `json:"email,omitempty"`
	CADirURL           string   `json:"ca,omitempty"`
	Staging            bool     `json:"staging,omitempty"`
	StorageDir         string   `json:"storage_dir,omitempty"`
	DNSProvider        string   `json:"dns_provider"`
	DNSCredentialVars  []string `json:"dns_credential_vars,omitempty"`
	PropagationSeconds int      `json:"propagation_seconds,omitempty"`
}

// acmeConfigPath is where the settings live, beside traefik.yaml and acme.json.
func acmeConfigPath(cfgPath string) string {
	return filepath.Join(filepath.Dir(cfgPath), acmeConfigFile)
}

// saveACMEConfig records cfg so a later launch without SKLP_SPACE_ACME can
// rebuild the same resolver. Best-effort by contract: failing to persist must
// not fail a deploy that is otherwise fine — it only costs the next relaunch
// its DNS challenge, which is what happens today anyway.
func saveACMEConfig(cfgPath string, cfg ingress.ACMEConfig) {
	if cfg.DNSProvider == "" {
		return
	}
	vars := make([]string, 0, len(cfg.DNSCredentials))
	for k := range cfg.DNSCredentials {
		vars = append(vars, k)
	}
	sort.Strings(vars)
	b, err := json.Marshal(persistedACME{
		Email:              cfg.Email,
		CADirURL:           cfg.CADirURL,
		Staging:            cfg.Staging,
		StorageDir:         cfg.StorageDir,
		DNSProvider:        cfg.DNSProvider,
		DNSCredentialVars:  vars,
		PropagationSeconds: int(cfg.PropagationTimeout / time.Second),
	})
	if err != nil {
		return
	}
	_ = atomicWrite(acmeConfigPath(cfgPath), b, 0o644)
}

// loadACMEConfig rebuilds the settings from disk, refilling credentials from
// the environment. It reports ok=false when nothing was persisted, or when a
// named credential var is missing from the environment: a dnsChallenge whose
// provider cannot authenticate fails every issuance, where httpChallenge still
// serves the non-wildcard hosts.
func loadACMEConfig(cfgPath string) (ingress.ACMEConfig, bool) {
	b, err := os.ReadFile(acmeConfigPath(cfgPath))
	if err != nil {
		return ingress.ACMEConfig{}, false
	}
	var p persistedACME
	if err := json.Unmarshal(b, &p); err != nil || p.DNSProvider == "" {
		return ingress.ACMEConfig{}, false
	}
	creds := make(map[string]string, len(p.DNSCredentialVars))
	for _, name := range p.DNSCredentialVars {
		v, ok := os.LookupEnv(name)
		if !ok || v == "" {
			return ingress.ACMEConfig{}, false
		}
		creds[name] = v
	}
	return ingress.ACMEConfig{
		Email:              p.Email,
		CADirURL:           p.CADirURL,
		Staging:            p.Staging,
		StorageDir:         p.StorageDir,
		DNSProvider:        p.DNSProvider,
		DNSCredentials:     creds,
		PropagationTimeout: time.Duration(p.PropagationSeconds) * time.Second,
	}, true
}

// acmeConfigError reports why a persisted config could not be used, for the
// operator-facing warning at launch.
func acmeConfigError(cfgPath string) error {
	if _, err := os.Stat(acmeConfigPath(cfgPath)); err != nil {
		return nil
	}
	return fmt.Errorf("gateway: persisted ACME config found but its DNS credentials are absent from the environment; falling back to httpChallenge (wildcard certificates will not issue)")
}
