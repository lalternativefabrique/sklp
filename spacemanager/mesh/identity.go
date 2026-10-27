// Package mesh brings up this node's side of the cross-VM WireGuard mesh
// (ADR-006): a stable node identity + WireGuard keypair, registration with
// the meshauthority service, and local application of the returned peer
// list to a wg0 interface.
package mesh

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// meshConfigDir is the GLOBAL mesh-identity directory, shared by every space
// on the host — one node has one mesh identity, independent of any space.
func meshConfigDir() string {
	return filepath.Join(configRoot(), "sklp", "mesh")
}

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

func nodeIDPath() string  { return filepath.Join(meshConfigDir(), "node-id") }
func privKeyPath() string { return filepath.Join(meshConfigDir(), "wg.key") }

// Identity is this node's stable mesh identity: a random ID (independent of
// any space or hostname, since either can change) and a WireGuard keypair.
type Identity struct {
	NodeID     string
	PrivateKey wgtypes.Key
	// SigningKey authenticates this node to meshauthority. Separate from
	// PrivateKey: WireGuard's is X25519 (key agreement), this one Ed25519.
	SigningKey ed25519.PrivateKey
}

// EnsureIdentity loads the node's persisted mesh identity, generating and
// persisting one on first run. Idempotent: a reboot reuses the same NodeID
// and private key, so the node's meshauthority registration and assigned
// mesh IP stay stable across restarts.
func EnsureIdentity() (Identity, error) {
	id, idErr := loadOrCreateNodeID()
	if idErr != nil {
		return Identity{}, fmt.Errorf("mesh node id: %w", idErr)
	}
	key, keyErr := loadOrCreatePrivateKey()
	if keyErr != nil {
		return Identity{}, fmt.Errorf("mesh private key: %w", keyErr)
	}
	signKey, signErr := loadOrCreateSigningKey()
	if signErr != nil {
		return Identity{}, fmt.Errorf("mesh signing key: %w", signErr)
	}
	return Identity{NodeID: id, PrivateKey: key, SigningKey: signKey}, nil
}

func loadOrCreateNodeID() (string, error) {
	if b, err := os.ReadFile(nodeIDPath()); err == nil {
		return string(b), nil
	} else if !os.IsNotExist(err) {
		return "", err
	}

	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate node id: %w", err)
	}
	id := hex.EncodeToString(buf)

	if err := os.MkdirAll(meshConfigDir(), 0o700); err != nil {
		return "", fmt.Errorf("create mesh config dir: %w", err)
	}
	if err := atomicWrite(nodeIDPath(), []byte(id), 0o600); err != nil {
		return "", err
	}
	return id, nil
}

func loadOrCreatePrivateKey() (wgtypes.Key, error) {
	if b, err := os.ReadFile(privKeyPath()); err == nil {
		return wgtypes.ParseKey(string(b))
	} else if !os.IsNotExist(err) {
		return wgtypes.Key{}, err
	}

	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return wgtypes.Key{}, fmt.Errorf("generate private key: %w", err)
	}

	if err := os.MkdirAll(meshConfigDir(), 0o700); err != nil {
		return wgtypes.Key{}, fmt.Errorf("create mesh config dir: %w", err)
	}
	if err := atomicWrite(privKeyPath(), []byte(key.String()), 0o600); err != nil {
		return wgtypes.Key{}, err
	}
	return key, nil
}

// atomicWrite writes data to a temp file in the target dir, chmods it, then
// renames it into place — so a reader never sees a half-written file.
func atomicWrite(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

func signKeyPath() string { return filepath.Join(meshConfigDir(), "sign.key") }

// loadOrCreateSigningKey returns this node's Ed25519 signing key, generating
// and persisting one on first run.
//
// The WireGuard key cannot do this job: it is X25519, a key agreement key, not
// a signature key. Deriving a signing key from it would misuse the same secret
// for two primitives. So the node carries a second, dedicated keypair next to
// wg.key, with the same lifetime — its public half is what meshauthority binds
// the node identity to, and every register/heartbeat is signed with the private
// half. Without it, any holder of the shared token could re-register another
// node's ID and take over its traffic.
func loadOrCreateSigningKey() (ed25519.PrivateKey, error) {
	if b, err := os.ReadFile(signKeyPath()); err == nil {
		raw, decErr := hex.DecodeString(string(b))
		if decErr != nil {
			return nil, fmt.Errorf("decode signing key: %w", decErr)
		}
		if len(raw) != ed25519.PrivateKeySize {
			return nil, fmt.Errorf("signing key: want %d bytes, got %d", ed25519.PrivateKeySize, len(raw))
		}
		return ed25519.PrivateKey(raw), nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate signing key: %w", err)
	}
	if err := os.MkdirAll(meshConfigDir(), 0o700); err != nil {
		return nil, fmt.Errorf("create mesh config dir: %w", err)
	}
	if err := atomicWrite(signKeyPath(), []byte(hex.EncodeToString(priv)), 0o600); err != nil {
		return nil, err
	}
	return priv, nil
}
