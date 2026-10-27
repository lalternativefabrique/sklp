package control

// token.go manages the GLOBAL Bearer token that authenticates callers on the
// NETWORK listener (the local Unix socket trusts its caller and needs none).
// The token is generated once on the first `--listen` and persisted 0600 under
// the global control dir, shared by every space, so a reboot / supervisor restart
// (or a space teardown) keeps the same token (no client/CI/UI reconfiguration).

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// TokenPath is the GLOBAL token file path (~/.config/sklp/control/token). The
// space arg is ignored (kept for call-site compatibility); the token is shared
// host-wide. Honours $XDG_CONFIG_HOME. Pure.
func TokenPath(space string) string {
	return filepath.Join(controlConfigDir(), "token")
}

// EnsureToken returns the host's network token, reusing the global stored one
// when present; otherwise it PROMOTES a legacy per-space token (so an already-
// deployed VM keeps the same token) or, failing that, generates and persists a
// fresh random one (32 bytes, base64url, no padding). Idempotent. 0600 atomic.
func EnsureToken(space string) (string, error) {
	path := TokenPath(space)
	if tok, err := readToken(path); err == nil {
		return tok, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	// Migration: promote a legacy per-space token to the global path.
	legacy := filepath.Join(spaceConfigDir(space), "token")
	if tok, err := readToken(legacy); err == nil {
		if err := writeTokenFile(path, tok); err != nil {
			return "", err
		}
		return tok, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	tok, err := generateToken()
	if err != nil {
		return "", err
	}
	if err := writeTokenFile(path, tok); err != nil {
		return "", err
	}
	return tok, nil
}

// LoadToken reads the persisted token without generating one. A missing file is
// returned as os.ErrNotExist (no network listener was ever opened on this host).
func LoadToken(space string) (string, error) {
	return readToken(TokenPath(space))
}

// generateToken makes a 32-byte cryptographically-random token, base64url
// (URL-safe, no padding) so it is a clean single argv/header token.
func generateToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func readToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(data))
	if tok == "" {
		return "", fmt.Errorf("token file %s is empty", path)
	}
	return tok, nil
}

// writeTokenFile writes the token 0600 via a temp file + rename, creating the
// parent config dir 0700.
func writeTokenFile(path, tok string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create control config dir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "token-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp token: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(tok + "\n"); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write temp token: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close temp token: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("chmod temp token: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("commit token: %w", err)
	}
	return nil
}

// RotateToken replaces the host's network token with a fresh one and returns
// it. The old token stops working as soon as the listener notices the file
// changed (see tokenSource), so the caller must record the new value first.
func RotateToken(space string) (string, error) { return RotateTokenAt(TokenPath(space)) }

func RotateTokenAt(path string) (string, error) {
	tok, err := generateToken()
	if err != nil {
		return "", err
	}
	if err := writeTokenFile(path, tok); err != nil {
		return "", err
	}
	return tok, nil
}
