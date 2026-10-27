// Package stackconfig reads and writes a stack's config file: the Pulumi-style
// single file that holds both plain config and encrypted secrets, keyed by name.
//
//	# .sklp/<stack>/config.yaml
//	encryptionsalt: <base64 salt>        # present once a secret is stored
//	encryptioniters: 600000              # KDF rounds this file was written with
//	config:
//	  PORT: "4100"                       # plain value
//	  AWS_ENDPOINT_URL: https://…        # plain value
//	secrets:
//	  SCW_SECRET_KEY:
//	    secure: <base64 ciphertext>      # AES-256-GCM, decrypted with the passphrase
//
// A plain entry is a bare scalar; a secret entry is a map with a single `secure`
// key. The ciphertext is committable — it is worthless without
// SKLP_SECRETS_PASSPHRASE. Decryption derives the key from that passphrase + the
// file's stored salt (secretcrypto), so a wrong passphrase fails loudly.
//
// The iteration count is stored per file, not compiled in. The count is part of
// the key derivation, so a file can only be read with the count it was written
// with; hard-coding it would mean raising the KDF cost later silently strands
// every existing file. A file with a salt but no recorded count predates this
// field and is read with secretcrypto.LegacyIters.
package stackconfig

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/lalternativefabrique/sklp/internal/secretcrypto"
	"gopkg.in/yaml.v3"
)

// PassphraseEnv is the environment variable holding the secrets passphrase
// (mirrors PULUMI_CONFIG_PASSPHRASE). No fallback: unset = secret ops fail.
const PassphraseEnv = "SKLP_SECRETS_PASSPHRASE"

// NewPassphraseEnv carries the replacement passphrase during a rotation. It is
// an env var rather than a flag so the value never lands in argv, where any
// local user could read it from /proc, nor in shell history.
const NewPassphraseEnv = "SKLP_SECRETS_PASSPHRASE_NEW"

// secureValue is the on-disk shape of a secret entry: {secure: <base64>}.
type secureValue struct {
	Secure string `yaml:"secure"`
}

// Config is a stack's parsed config file.
type Config struct {
	// Salt is the base64 PBKDF2 salt; empty until the first secret is set.
	Salt string `yaml:"encryptionsalt,omitempty"`
	// Iters is the PBKDF2 round count this file's secrets were sealed with.
	// Zero on a file written before the field existed; KDFIters resolves that
	// to secretcrypto.LegacyIters.
	Iters int `yaml:"encryptioniters,omitempty"`
	// Entries preserves insertion/sorted order via Keys; Plain and Secret hold
	// the two value kinds. A name is in exactly one of them.
	Plain  map[string]string `yaml:"-"`
	Secret map[string]string `yaml:"-"` // name -> base64 ciphertext
}

// KDFIters is the round count to derive this file's key with: the recorded one,
// or the legacy count for a file written before the field existed. Reading with
// any other count fails exactly like a wrong passphrase.
func (c *Config) KDFIters() int {
	if c.Iters > 0 {
		return c.Iters
	}
	return secretcrypto.LegacyIters
}

// rawConfig is the YAML wire shape: plain values under `config`, sealed ones
// under `secrets`. A sealed entry still found under `config` is read as a
// secret, so files written before the split keep working.
type rawConfig struct {
	Salt    string                 `yaml:"encryptionsalt,omitempty"`
	Iters   int                    `yaml:"encryptioniters,omitempty"`
	Config  map[string]yaml.Node   `yaml:"config"`
	Secrets map[string]secureValue `yaml:"secrets,omitempty"`
}

// Load reads the config file at path. A missing file returns an empty Config
// (not an error): a stack may have no config yet.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &Config{Plain: map[string]string{}, Secret: map[string]string{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var raw rawConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	c := &Config{Salt: raw.Salt, Iters: raw.Iters, Plain: map[string]string{}, Secret: map[string]string{}}
	for name, sv := range raw.Secrets {
		if sv.Secure != "" {
			c.Secret[name] = sv.Secure
		}
	}
	for name, node := range raw.Config {
		// A secret is a mapping node with a `secure` key; anything else is plain.
		if node.Kind == yaml.MappingNode {
			var sv secureValue
			if err := node.Decode(&sv); err == nil && sv.Secure != "" {
				c.Secret[name] = sv.Secure
				continue
			}
		}
		var s string
		if err := node.Decode(&s); err != nil {
			return nil, fmt.Errorf("config %q: %w", name, err)
		}
		c.Plain[name] = s
	}
	return c, nil
}

// Save writes the config file at path atomically (temp + rename), 0600 (the
// ciphertext is safe but the file may carry plain config the operator prefers
// private). Keys are sorted for a stable, diff-friendly file.
func Save(path string, c *Config) error {
	raw := rawConfig{Salt: c.Salt, Iters: c.Iters, Config: map[string]yaml.Node{}}
	for name, v := range c.Plain {
		var n yaml.Node
		_ = n.Encode(v)
		raw.Config[name] = n
	}
	if len(c.Secret) > 0 {
		raw.Secrets = map[string]secureValue{}
		for name, ct := range c.Secret {
			raw.Secrets[name] = secureValue{Secure: ct}
		}
	}
	out, err := yaml.Marshal(raw)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// SetPlain sets a plain (unencrypted) value, removing any secret of the name.
func (c *Config) SetPlain(name, value string) {
	delete(c.Secret, name)
	c.Plain[name] = value
}

// SetSecret encrypts value under the passphrase and stores it as a secret,
// removing any plain entry of the name. The salt is created on first use and
// reused thereafter so every secret in the file shares one key.
func (c *Config) SetSecret(name, value, passphrase string) error {
	if err := c.ensureSalt(); err != nil {
		return err
	}
	salt, err := base64.StdEncoding.DecodeString(c.Salt)
	if err != nil {
		return fmt.Errorf("decode salt: %w", err)
	}
	ci, err := secretcrypto.NewCipherFromPassphrase(passphrase, salt, c.KDFIters())
	if err != nil {
		return err
	}
	blob, err := ci.Encrypt(value)
	if err != nil {
		return err
	}
	delete(c.Plain, name)
	c.Secret[name] = base64.StdEncoding.EncodeToString(blob)
	return nil
}

// Delete removes a name from either map. Returns whether it existed.
func (c *Config) Delete(name string) bool {
	_, p := c.Plain[name]
	_, s := c.Secret[name]
	delete(c.Plain, name)
	delete(c.Secret, name)
	return p || s
}

// ensureSalt allocates the file's PBKDF2 salt and pins its iteration count on
// the first secret. Both are set together: a salt without a recorded count
// reads back as a legacy file, which would silently seal new secrets at the old
// cost. An existing file keeps the count it was written with — raising it here
// would make its own secrets undecryptable.
func (c *Config) ensureSalt() error {
	if c.Salt != "" {
		return nil
	}
	salt, err := secretcrypto.NewSalt()
	if err != nil {
		return err
	}
	c.Salt = base64.StdEncoding.EncodeToString(salt)
	c.Iters = secretcrypto.DefaultIters
	return nil
}

// Keys returns every configured name (plain + secret), sorted.
func (c *Config) Keys() []string {
	out := make([]string, 0, len(c.Plain)+len(c.Secret))
	for k := range c.Plain {
		out = append(out, k)
	}
	for k := range c.Secret {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// IsSecret reports whether name is stored encrypted.
func (c *Config) IsSecret(name string) bool {
	_, ok := c.Secret[name]
	return ok
}

// Rotate re-seals every secret under newPassphrase, returning the rewritten
// config. Plain values are copied untouched.
//
// A fresh salt is drawn, which is the point: reusing the old one would change
// the passphrase while leaving half of the key material unchanged, and
// SetSecret alone cannot do it (ensureSalt is a no-op once a salt exists, so
// re-setting values on the same Config would silently keep the old salt).
//
// The new file is sealed at DefaultIters, so rotating is also how a file
// written under the old 100k count moves to the current one.
//
// The receiver is left untouched — the caller decides whether to persist the
// result, so a failure midway cannot leave a half-rotated file behind.
func (c *Config) Rotate(oldPassphrase, newPassphrase string) (*Config, error) {
	if newPassphrase == "" {
		return nil, errors.New("new passphrase is empty")
	}
	if newPassphrase == oldPassphrase {
		return nil, errors.New("new passphrase is identical to the current one")
	}

	resolved, err := c.Resolve(oldPassphrase)
	if err != nil {
		return nil, err
	}

	out := &Config{Plain: map[string]string{}, Secret: map[string]string{}}
	for name, v := range c.Plain {
		out.Plain[name] = v
	}
	// Re-seal in a deterministic order so a failure names the same key on a
	// retry rather than an arbitrary one.
	names := make([]string, 0, len(c.Secret))
	for name := range c.Secret {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := out.SetSecret(name, resolved[name], newPassphrase); err != nil {
			return nil, fmt.Errorf("secret %q: re-seal: %w", name, err)
		}
	}
	return out, nil
}

// Resolve returns every config value with secrets DECRYPTED, ready to inject as
// environment for ${VAR} expansion at deploy. passphrase is required only when
// the file has secrets; a plain-only file resolves with an empty passphrase.
// A wrong passphrase surfaces as a decrypt error naming the offending key.
func (c *Config) Resolve(passphrase string) (map[string]string, error) {
	return c.ResolveWanted(passphrase, nil)
}

// ResolveWanted is Resolve restricted to the secrets the caller asks for:
// plain values always come through, while a secret is decrypted only when
// wanted[name] is true. A nil map decrypts everything, which is what
// `sklp config` and `sklp deploy` need.
//
// The passphrase is the point: a caller that wants no encrypted value never
// derives a key, so it never needs one. That is what lets a CI pipeline
// declaring no secret run on a machine without the vault key, instead of
// failing over secrets it would never read.
func (c *Config) ResolveWanted(passphrase string, wanted map[string]bool) (map[string]string, error) {
	out := make(map[string]string, len(c.Plain)+len(c.Secret))
	for k, v := range c.Plain {
		out[k] = v
	}
	needed := make(map[string]string, len(c.Secret))
	for name, b64 := range c.Secret {
		if wanted == nil || wanted[name] {
			needed[name] = b64
		}
	}
	if len(needed) == 0 {
		return out, nil
	}
	salt, err := base64.StdEncoding.DecodeString(c.Salt)
	if err != nil {
		return nil, fmt.Errorf("decode salt: %w", err)
	}
	ci, err := secretcrypto.NewCipherFromPassphrase(passphrase, salt, c.KDFIters())
	if err != nil {
		return nil, err
	}
	for name, b64 := range needed {
		blob, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("secret %q: decode: %w", name, err)
		}
		val, err := ci.Decrypt(blob)
		if err != nil {
			return nil, fmt.Errorf("secret %q: %w", name, err)
		}
		out[name] = val
	}
	return out, nil
}
