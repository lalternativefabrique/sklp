// Package registry holds the sklp CLI's own registry credentials: the file
// `sklp registry login` writes and every push reads back.
//
// It is deliberately NOT part of internal/config. That file is mounted
// read-only into the Landlock sandbox of every CI step (plan.Sandbox's
// SklpUserConfig) and copied onto nodes one-file-per-VM (SKLP_CONFIG), so a
// push credential stored there would be readable by any `run:` a pipeline
// declares. This file is never granted to a sandbox and never leaves the
// machine that ran the login.
//
// sklp does not read or write ~/.docker/config.json. A credential this file
// does not carry does not exist, so a revoked or stale docker login can no
// longer authenticate a push behind sklp's back.
//
// Location follows the XDG Base Directory spec, alongside config.yaml:
//
//	$XDG_CONFIG_HOME/sklp/registries.yaml     (if XDG_CONFIG_HOME is set)
//	$HOME/.config/sklp/registries.yaml        (XDG default)
//	$HOME/.sklp/registries.yaml               (fallback)
//
// Saves are atomic (tmp + rename) and the file is chmod'd to 0600.
package registry

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// EnvFile points directly at a registries file, bypassing the XDG lookup.
// Symmetric with SKLP_CONFIG: one file per node/VM.
const EnvFile = "SKLP_REGISTRIES"

// ErrNoCredential is returned by Resolve when the file carries no entry for
// the requested host. Callers match on it to print the `sklp registry login`
// hint rather than a generic failure.
var ErrNoCredential = errors.New("no registry credential")

// File is the on-disk shape of registries.yaml.
type File struct {
	Version    int                   `yaml:"version"`
	Registries map[string]Credential `yaml:"registries,omitempty"`
}

// Credential is one registry's login, keyed by host in File.Registries.
//
// Auth and CredentialCommand are alternatives: Auth stores base64(user:pass)
// in the file, CredentialCommand names a lookup command whose stdout is the
// password. The command form keeps the secret in whatever store the user
// already trusts (libsecret, pass, a cloud vault) — sklp implements no vault
// of its own, the same reasoning as config.Secrets.PassphraseCommand.
type Credential struct {
	Username          string `yaml:"username"`
	Auth              string `yaml:"auth,omitempty"`
	CredentialCommand string `yaml:"credential_command,omitempty"`
}

// Resolved is a credential with its password materialised, ready for a login.
type Resolved struct {
	Host     string
	Username string
	Password string
}

// Path returns the resolved registries file path, creating only its parent
// directory. It mirrors config.Path so both files land side by side.
func Path() (string, error) {
	if p := os.Getenv(EnvFile); p != "" {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return "", fmt.Errorf("create dir for %s %s: %w", EnvFile, p, err)
		}
		return p, nil
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		dir := filepath.Join(xdg, "sklp")
		if err := os.MkdirAll(dir, 0o700); err == nil {
			return filepath.Join(dir, "registries.yaml"), nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	xdgDefault := filepath.Join(home, ".config", "sklp")
	if err := os.MkdirAll(xdgDefault, 0o700); err == nil {
		return filepath.Join(xdgDefault, "registries.yaml"), nil
	}
	fallback := filepath.Join(home, ".sklp")
	if err := os.MkdirAll(fallback, 0o700); err != nil {
		return "", fmt.Errorf("create fallback dir %s: %w", fallback, err)
	}
	return filepath.Join(fallback, "registries.yaml"), nil
}

// Load reads the registries file. A missing file yields an empty File and the
// path it would be written to, not an error: no login yet is a normal state.
func Load() (*File, string, error) {
	path, err := Path()
	if err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &File{Version: 1, Registries: map[string]Credential{}}, path, nil
	}
	if err != nil {
		return nil, path, fmt.Errorf("read %s: %w", path, err)
	}
	if runtime.GOOS != "windows" {
		if info, statErr := os.Stat(path); statErr == nil {
			if mode := info.Mode().Perm(); mode&0o077 != 0 {
				fmt.Fprintf(os.Stderr,
					"warning: %s has loose permissions (mode %#o); recommended 0600\n",
					path, mode)
			}
		}
	}
	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, path, fmt.Errorf("parse %s: %w", path, err)
	}
	if f.Registries == nil {
		f.Registries = map[string]Credential{}
	}
	if f.Version == 0 {
		f.Version = 1
	}
	return &f, path, nil
}

// Save writes the file atomically: marshal → tmp → chmod 0600 → rename.
func (f *File) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create dir: %w", err)
	}
	if f.Version == 0 {
		f.Version = 1
	}
	data, err := yaml.Marshal(f)
	if err != nil {
		return fmt.Errorf("marshal registries: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".registries.yaml.tmp-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		cleanup()
		return fmt.Errorf("chmod temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("rename: %w", err)
	}
	_ = os.Chmod(path, 0o600)
	return nil
}

// Set upserts a host's credential.
func (f *File) Set(host string, c Credential) {
	if f.Registries == nil {
		f.Registries = map[string]Credential{}
	}
	f.Registries[strings.TrimSpace(host)] = c
}

// Delete drops a host, reporting whether it was present.
func (f *File) Delete(host string) bool {
	host = strings.TrimSpace(host)
	if _, ok := f.Registries[host]; !ok {
		return false
	}
	delete(f.Registries, host)
	return true
}

// Hosts returns the configured hosts in sorted order.
func (f *File) Hosts() []string {
	out := make([]string, 0, len(f.Registries))
	for h := range f.Registries {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// EncodeAuth builds the base64(user:pass) blob stored under Credential.Auth.
func EncodeAuth(username, password string) string {
	return base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
}

// decodeAuth splits a base64(user:pass) blob back into its two halves.
func decodeAuth(auth string) (string, string, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(auth))
	if err != nil {
		return "", "", fmt.Errorf("decode auth: %w", err)
	}
	user, pass, ok := strings.Cut(string(raw), ":")
	if !ok {
		return "", "", errors.New("decode auth: expected user:password")
	}
	return user, pass, nil
}

// Resolve materialises the credential for host, running its
// credential_command when it has one. Returns ErrNoCredential when the file
// carries no entry, so the caller can point at `sklp registry login`.
func Resolve(host string) (Resolved, error) {
	f, _, err := Load()
	if err != nil {
		return Resolved{}, err
	}
	return f.Resolve(host)
}

// Resolve is Load-free: it materialises host's credential from an already
// loaded file, so a run that pushes several images reads the file once.
func (f *File) Resolve(host string) (Resolved, error) {
	host = strings.TrimSpace(host)
	c, ok := f.Registries[host]
	if !ok {
		return Resolved{}, fmt.Errorf("%w for %q", ErrNoCredential, host)
	}
	username := strings.TrimSpace(c.Username)
	switch {
	case c.CredentialCommand != "":
		password, err := runCredentialCommand(c.CredentialCommand)
		if err != nil {
			return Resolved{}, fmt.Errorf("registry %s: credential_command: %w", host, err)
		}
		if username == "" {
			return Resolved{}, fmt.Errorf("registry %s: username is required alongside credential_command", host)
		}
		return Resolved{Host: host, Username: username, Password: password}, nil
	case c.Auth != "":
		user, password, err := decodeAuth(c.Auth)
		if err != nil {
			return Resolved{}, fmt.Errorf("registry %s: %w", host, err)
		}
		if username == "" {
			username = user
		}
		return Resolved{Host: host, Username: username, Password: password}, nil
	default:
		return Resolved{}, fmt.Errorf("registry %s: entry has neither auth nor credential_command", host)
	}
}

// runCredentialCommand executes a lookup command through the shell and
// returns its stdout as the password. Shell so the configured string can be a
// pipeline the user already runs by hand. stderr is passed through: a locked
// keyring must be able to prompt.
func runCredentialCommand(command string) (string, error) {
	cmd := exec.Command("sh", "-c", command)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%q: %w", command, err)
	}
	password := strings.TrimRight(string(out), "\r\n")
	if password == "" {
		return "", fmt.Errorf("%q produced no output", command)
	}
	return password, nil
}
