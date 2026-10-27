package sklet

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/lalternativefabrique/sklp/internal/xdg"
	"github.com/lalternativefabrique/sklp/sklet/publicnic"
	"github.com/lalternativefabrique/sklp/spacemanager"
	"github.com/lalternativefabrique/sklp/spacemanager/mesh"
	"gopkg.in/yaml.v3"
)

// NodeConfig is everything a node's agent unit is rendered from: the flags
// `sklet agent` takes and the node-owned environment the supervisor reads.
// Secrets never live here — they sit in the EnvironmentFile next to it.
type NodeConfig struct {
	Listen          string            `yaml:"listen"`
	GatewayMode     string            `yaml:"gateway_mode"`
	Advertise       string            `yaml:"advertise,omitempty"`
	AdvertiseVia    string            `yaml:"advertise_via,omitempty"`
	NodeRelayDomain string            `yaml:"node_relay_domain,omitempty"`
	DataRoot        string            `yaml:"data_root,omitempty"`
	ACMEEmail       string            `yaml:"acme_email,omitempty"`
	OTLPEndpoint    string            `yaml:"otlp_endpoint,omitempty"`
	Mesh            NodeMesh          `yaml:"mesh,omitempty"`
	Policy          NodePolicy        `yaml:"policy,omitempty"`
	PublicNIC       *publicnic.Config `yaml:"public_nic,omitempty"`
}

type NodeMesh struct {
	AuthorityURL         string `yaml:"authority_url,omitempty"`
	AuthorityURLInternal string `yaml:"authority_url_internal,omitempty"`
	Endpoint             string `yaml:"endpoint,omitempty"`
	CertFingerprint      string `yaml:"cert_fingerprint,omitempty"`
}

type NodePolicy struct {
	TrustedRegistries  []string `yaml:"trusted_registries,omitempty"`
	RequireImageDigest bool     `yaml:"require_image_digest,omitempty"`
	ACMEStorageDir     string   `yaml:"acme_storage_dir,omitempty"`
}

const (
	nodeConfigFile = "node.yaml"
	nodeEnvFile    = "node.env"
	agentUnitName  = "sklet-agent.service"
)

func nodeConfigDir() string {
	if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
		return filepath.Join(base, "sklp")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "sklp-config", "sklp")
	}
	return filepath.Join(home, ".config", "sklp")
}

// NodeConfigPath is where an operator edits the node, and what `sklet host
// configure` renders from.
func NodeConfigPath() string { return filepath.Join(nodeConfigDir(), nodeConfigFile) }

// NodeEnvPath holds the node's secrets (mesh token, log ingest key) as a 0600
// dotenv the unit loads, so they never sit in the unit file itself.
func NodeEnvPath() string { return filepath.Join(nodeConfigDir(), nodeEnvFile) }

// AgentUnitPath is the per-user unit, which systemd prefers over the global
// /etc/systemd/user copy cloud-init may have left: an unprivileged
// `sklet host configure` can therefore take over an existing node.
func AgentUnitPath() string {
	if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
		return filepath.Join(base, "systemd", "user", agentUnitName)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "systemd-user", agentUnitName)
	}
	return filepath.Join(home, ".config", "systemd", "user", agentUnitName)
}

func LoadNodeConfig(path string) (*NodeConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseNodeConfig(raw)
}

// ParseNodeConfig is strict: an unknown key is a typo that would otherwise
// silently leave a setting at its default.
func ParseNodeConfig(raw []byte) (*NodeConfig, error) {
	var cfg NodeConfig
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse node config: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *NodeConfig) validate() error {
	if c.Listen == "" {
		return fmt.Errorf("node config: listen is required (e.g. \":6464\")")
	}
	if err := spacemanager.ValidateGatewayMode(c.GatewayMode); err != nil {
		return fmt.Errorf("node config: %w", err)
	}
	if c.Mesh.AuthorityURL != "" && c.Mesh.Endpoint == "" {
		return fmt.Errorf("node config: mesh.endpoint is required when mesh.authority_url is set")
	}
	if c.Mesh.AuthorityURL != "" {
		if err := mesh.CheckAuthority(c.Mesh.AuthorityURL, c.Mesh.CertFingerprint); err != nil {
			return fmt.Errorf("node config: %w", err)
		}
	}
	if c.Advertise != "" && c.AdvertiseVia != "" {
		return fmt.Errorf("node config: advertise and advertise_via are exclusive")
	}
	for _, v := range []string{c.Listen, c.GatewayMode, c.Advertise, c.AdvertiseVia, c.NodeRelayDomain, c.DataRoot, c.ACMEEmail, c.OTLPEndpoint, c.Mesh.AuthorityURL, c.Mesh.AuthorityURLInternal, c.Mesh.Endpoint, c.Mesh.CertFingerprint, c.Policy.ACMEStorageDir} {
		if strings.ContainsAny(v, " \t\n\"'") {
			return fmt.Errorf("node config: value %q cannot contain whitespace or quotes", v)
		}
	}
	if c.PublicNIC != nil {
		if err := c.PublicNIC.Validate(); err != nil {
			return fmt.Errorf("node config: %w", err)
		}
	}
	for _, r := range c.Policy.TrustedRegistries {
		if strings.ContainsAny(r, " \t\n\"',") {
			return fmt.Errorf("node config: registry %q cannot contain whitespace, quotes or commas", r)
		}
	}
	return nil
}

// AgentArgs is the `sklet agent` argv the unit runs.
func (c *NodeConfig) AgentArgs() []string {
	args := []string{"agent", "--listen", c.Listen, "--gateway-mode", c.GatewayMode}
	add := func(flag, v string) {
		if v != "" {
			args = append(args, flag, v)
		}
	}
	add("--advertise", c.Advertise)
	add("--advertise-via", c.AdvertiseVia)
	add("--node-relay-domain", c.NodeRelayDomain)
	if c.Mesh.AuthorityURL != "" {
		add("--mesh-authority-url", c.Mesh.AuthorityURL)
		add("--mesh-authority-url-internal", c.Mesh.AuthorityURLInternal)
		add("--mesh-endpoint", c.Mesh.Endpoint)
		add("--mesh-cert-fingerprint", c.Mesh.CertFingerprint)
	}
	if c.PublicNIC != nil {
		nic := c.PublicNIC.WithDefaults()
		add("--public-nic-interface", nic.Interface)
		add("--public-nic-source", nic.Source)
		add("--public-nic-table", strconv.Itoa(nic.Table))
		add("--public-nic-mark", strconv.FormatUint(uint64(nic.Mark), 10))
	}
	return args
}

// Environment is the node-owned environment the unit exports: what the
// supervisor and the space units it spawns read the node's policy from.
func (c *NodeConfig) Environment() []string {
	var env []string
	add := func(k, v string) {
		if v != "" {
			env = append(env, k+"="+v)
		}
	}
	add(xdg.DataRootEnv, c.DataRoot)
	add("SKLP_GATEWAY_ACME_EMAIL", c.ACMEEmail)
	add("OTEL_EXPORTER_OTLP_ENDPOINT", c.OTLPEndpoint)
	add(spacemanager.TrustedRegistriesEnv, strings.Join(c.Policy.TrustedRegistries, ","))
	if c.Policy.RequireImageDigest {
		add(spacemanager.RequireImageDigestEnv, "1")
	}
	add(spacemanager.ACMEStorageDirEnv, c.Policy.ACMEStorageDir)
	return env
}

// RenderAgentUnit renders the sklet-agent user unit. Restart=always, not
// on-failure: systemd counts SIGHUP/SIGINT/SIGTERM as clean exits, and a stray
// HUP once took every space on prod down with nothing relaunching them.
// RestartPreventExitStatus=78 still keeps a config error from looping.
func (c *NodeConfig) RenderAgentUnit(selfExe, envFile string) string {
	var b strings.Builder
	b.WriteString("[Unit]\n")
	b.WriteString("Description=sklet control-plane node (control API, spaces supervisor)\n")
	b.WriteString("After=network-online.target\n")
	b.WriteString("Wants=network-online.target\n\n")
	b.WriteString("[Service]\n")
	b.WriteString("Type=simple\n")
	for _, e := range c.Environment() {
		b.WriteString("Environment=" + e + "\n")
	}
	if envFile != "" {
		b.WriteString("EnvironmentFile=-" + envFile + "\n")
	}
	b.WriteString("ExecStart=" + selfExe + " " + strings.Join(c.AgentArgs(), " ") + "\n")
	b.WriteString("Restart=always\n")
	b.WriteString("RestartSec=5\n")
	b.WriteString("RestartPreventExitStatus=78\n\n")
	b.WriteString("[Install]\n")
	b.WriteString("WantedBy=default.target\n")
	return b.String()
}
