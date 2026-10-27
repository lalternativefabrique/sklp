package control

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lalternativefabrique/sklp/spacemanager"
)

// manifest persistence — survive reboot, independence from the cwd.
//
// At each deploy/apply the resolved space (services + meta) is written to a
// STABLE path so the systemd-supervised __supervise can read it at boot even
// when the original repo cwd no longer exists. The on-disk shape reuses the
// same applySpec the control API marshals, so the manifest and the apply body
// are one format (one source of truth). This file is gated on embedded_runtime
// because it names the deploy types; protocol.go/client.go stay tag-free.

// ManifestPath is the deterministic per-space manifest path:
// ~/.config/sklp/spaces/<space>/manifest.json. It honours $XDG_CONFIG_HOME.
// Pure: it does not touch the filesystem.
func ManifestPath(space string) string {
	return filepath.Join(spaceConfigDir(space), "manifest.json")
}

// SpaceIsPersisted reports whether space has a manifest on this node, i.e. a
// deploy created it at some point — true for a STOPPED space, which has no
// supervisor and so cannot be found in the live registry. It is what tells a
// space that merely stopped apart from a name nobody ever deployed.
func SpaceIsPersisted(space string) bool {
	if space == "" {
		return false
	}
	_, err := os.Stat(ManifestPath(space))
	return err == nil
}

// spaceConfigDir is the per-space config directory.
func spaceConfigDir(space string) string {
	return filepath.Join(configRoot(), "sklp", "spaces", space)
}

// controlConfigDir is the GLOBAL control-identity directory, shared by every
// space on the host: the TLS cert/key + Bearer token live here so a space
// teardown never invalidates a client's pinned fingerprint/token.
func controlConfigDir() string {
	return filepath.Join(configRoot(), "sklp", "control")
}

// configRoot resolves the XDG config base ($XDG_CONFIG_HOME, else ~/.config).
// On a homeless env it falls back to the OS temp dir so the path is well-formed.
func configRoot() string {
	if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
		return base
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "sklp-config")
	}
	return filepath.Join(home, ".config")
}

// SaveManifest persists the resolved space manifest atomically (temp + rename)
// under the deterministic path, creating the parent dirs 0700. Reuses applySpec
// so the persisted shape matches the apply wire format exactly.
func SaveManifest(space string, services []spacemanager.Service, meta spacemanager.SpaceMeta) error {
	dir := spaceConfigDir(space)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create space config dir: %w", err)
	}
	meta.EnvValues = nil
	data, err := json.MarshalIndent(applySpec{Services: services, Meta: meta}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	path := filepath.Join(dir, "manifest.json")
	tmp, err := os.CreateTemp(dir, "manifest-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp manifest: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write temp manifest: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close temp manifest: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("chmod temp manifest: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("commit manifest: %w", err)
	}
	return nil
}

// RemoveSpaceArtifacts deletes the per-space config dir (manifest, pidfile,
// listen, gateway-driver, supervisor.log) so a torn-down space does NOT resurrect
// at the next boot: a systemd-relaunched __supervise finds no manifest and exits.
// The control identity (cert/key/token) lives under controlConfigDir, NOT here,
// so it survives any teardown by construction — a client's pinned fingerprint and
// token stay valid. Best-effort and idempotent — a missing dir is not an error.
// Call it ONLY after the supervisor is confirmed stopped, so a half-failed down
// never loses a still-running space's manifest.
func RemoveSpaceArtifacts(space string) error {
	if err := validSpaceName(space); err != nil {
		return err
	}
	if err := os.RemoveAll(spaceConfigDir(space)); err != nil {
		return fmt.Errorf("remove space artifacts for %q: %w", space, err)
	}
	return nil
}

// ListenAddrPath is the per-space network-listen address file. Its presence
// means the space was deployed with `--listen :PORT`; the supervisor re-reads
// it at boot so the network API comes back automatically. Honours $XDG_CONFIG_HOME.
func ListenAddrPath(space string) string {
	return filepath.Join(spaceConfigDir(space), "listen")
}

// SaveListenAddr persists the network listen address (e.g. ":9443") 0600 so a
// later __supervise (reboot, systemd restart) reopens the network API on the same
// port without re-passing --listen. Atomic. An empty addr is a no-op.
func SaveListenAddr(space, addr string) error {
	if addr == "" {
		return nil
	}
	dir := spaceConfigDir(space)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create space config dir: %w", err)
	}
	path := ListenAddrPath(space)
	tmp, err := os.CreateTemp(dir, "listen-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp listen addr: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(addr + "\n"); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write temp listen addr: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close temp listen addr: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("chmod temp listen addr: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("commit listen addr: %w", err)
	}
	return nil
}

// LoadListenAddr reads the persisted network listen address. A missing file
// returns ("", nil): the space has no network listener (socket-only).
func LoadListenAddr(space string) (string, error) {
	data, err := os.ReadFile(ListenAddrPath(space))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read listen addr for space %q: %w", space, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// GatewayDriverPath is the per-space file recording the gateway driver the space
// was deployed with, persisted next to the manifest so a reboot / systemd restart
// reconstructs the SAME driver instead of falling back to the default. Honours
// $XDG_CONFIG_HOME.
func GatewayDriverPath(space string) string {
	return filepath.Join(spaceConfigDir(space), "gateway-driver")
}

// SaveGatewayDriver persists the gateway driver name 0600 (atomic) so a later
// __supervise (reboot, systemd restart) brings up the right driver without the
// DeployConfig. An empty name is a no-op (leave any prior value untouched).
func SaveGatewayDriver(space, driver string) error {
	if driver == "" {
		return nil
	}
	dir := spaceConfigDir(space)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create space config dir: %w", err)
	}
	path := GatewayDriverPath(space)
	tmp, err := os.CreateTemp(dir, "gateway-driver-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp gateway driver: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(driver + "\n"); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write temp gateway driver: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close temp gateway driver: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("chmod temp gateway driver: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("commit gateway driver: %w", err)
	}
	return nil
}

// LoadGatewayDriver reads the persisted gateway driver name. A missing file
// returns ("", nil): the caller falls back to the default driver.
func LoadGatewayDriver(space string) (string, error) {
	data, err := os.ReadFile(GatewayDriverPath(space))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read gateway driver for space %q: %w", space, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// LoadManifest reads the persisted manifest of a space. It is what __supervise
// reads at boot instead of the cwd. A missing file is a clear error (the space
// was never deployed on this host).
func LoadManifest(space string) ([]spacemanager.Service, spacemanager.SpaceMeta, error) {
	path := ManifestPath(space)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, spacemanager.SpaceMeta{}, fmt.Errorf("read manifest for space %q (%s): %w", space, path, err)
	}
	var spec applySpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return nil, spacemanager.SpaceMeta{}, fmt.Errorf("decode manifest for space %q: %w", space, err)
	}
	if len(spec.Services) == 0 {
		return nil, spacemanager.SpaceMeta{}, fmt.Errorf("manifest for space %q declares no services", space)
	}
	// Load the secrets a remote apply resolved into this process's environment
	// BEFORE resolving the refs. Done here rather than left to the unit's
	// EnvironmentFile= so every restore path benefits — `sklet agent` runs from a
	// unit written by cloud-init, which knows nothing of this file.
	//
	// The host env wins: a value already set (an operator's dotenv, the local
	// path) is authoritative over what a past apply happened to resolve.
	return spacemanager.ResolveEnvRefsWith(spec.Services, spec.Meta.EnvRefs, SpaceSecretLookup(space)), spec.Meta, nil
}

// ListPersistedSpaces returns the names of every space with a manifest on
// this host — ~/.config/sklp/spaces/<name>/manifest.json — sorted. This is
// the ON-DISK inventory (a space that was ever deployed and never torn
// down), independent of which sockets currently answer (control.ListSpaces)
// or which supervisor process is running. `sklet agent` uses it at boot to
// discover every space a prior remote deploy already created, so a restart
// resurrects ALL of them, not just one fixed name. A missing spaces dir
// (fresh node) yields an empty list, not an error.
func ListPersistedSpaces() ([]string, error) {
	dir := filepath.Join(configRoot(), "sklp", "spaces")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list persisted spaces: %w", err)
	}
	var spaces []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, e.Name(), "manifest.json")); err != nil {
			continue // a bare/incomplete space dir (e.g. mid-write) is not a space
		}
		spaces = append(spaces, e.Name())
	}
	sort.Strings(spaces)
	return spaces, nil
}

// loadSpaceEnv reads the space's persisted secrets into this process's
// environment, without overriding a variable the host already sets. A missing
// file is the normal case for a locally-deployed space and is not an error.
// SpaceSecretLookup resolves a secret name for one space: its persisted env
// file first, the process environment only as a fallback for local runs.
func SpaceSecretLookup(space string) func(string) (string, bool) {
	vars, err := ReadSpaceEnv(space)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sklet: warning: could not read resolved secrets for space %q: %v\n", space, err)
	}
	return func(name string) (string, bool) {
		if v, ok := vars[name]; ok {
			return v, true
		}
		return os.LookupEnv(name)
	}
}

// ReadSpaceEnv returns the secrets persisted for one space, never touching the
// process environment: several spaces on a node declare the same variable
// names with different values, so a shared environment cannot hold them.
func ReadSpaceEnv(space string) (map[string]string, error) {
	data, err := os.ReadFile(SpaceEnvPath(space))
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	vars := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found || key == "" {
			continue
		}
		vars[key] = value
	}
	return vars, nil
}

// ReadPersistedManifest reads the manifest of a space exactly as it sits on
// disk: env refs stay stripped and registry passwords stay blank. It is the
// read path an INSPECTING caller (the skalpai UI) needs, and deliberately not
// LoadManifest — that one resolves the refs back from the supervisor's own
// environment for the boot path, which would hand every secret to whoever can
// reach the control API.
func ReadPersistedManifest(space string) ([]spacemanager.Service, spacemanager.SpaceMeta, error) {
	data, err := os.ReadFile(ManifestPath(space))
	if err != nil {
		return nil, spacemanager.SpaceMeta{}, fmt.Errorf("read manifest for space %q: %w", space, err)
	}
	var spec applySpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return nil, spacemanager.SpaceMeta{}, fmt.Errorf("decode manifest for space %q: %w", space, err)
	}
	return spec.Services, spec.Meta, nil
}
