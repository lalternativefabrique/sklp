// Package secretcrypto encrypts and decrypts config secrets with AES-256-GCM,
// keyed by a passphrase (Pulumi-style: a passphrase + a per-file salt derive the
// key via PBKDF2). The Encrypt/Decrypt core mirrors core's
// project_secret/infrastructure/crypto.go (AES-256-GCM, nonce||ciphertext blob)
// — proven crypto, copied rather than coupled across the two Go modules. The
// passphrase derivation is the sklp-specific layer core does not have (core uses
// a raw 32-byte key from env; sklp uses a human passphrase from
// SKLP_SECRETS_PASSPHRASE, like PULUMI_CONFIG_PASSPHRASE).
package secretcrypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
)

// DefaultIters is the PBKDF2-SHA256 iteration count used for NEW files. The
// count is the only thing standing between a guessable passphrase and an
// offline attack: the ciphertext is designed to be committed, so an attacker
// who reads the repo can try candidate passphrases on their own hardware with
// no rate limit. Each iteration is a cost they pay per guess and the owner pays
// once. 600k is the current OWASP floor for PBKDF2-SHA256 (~0.2s to derive).
//
// Callers pass the count explicitly and writers persist it next to the salt, so
// this value can be raised later without stranding existing files — the
// iteration count travels with the ciphertext that used it.
const DefaultIters = 600_000

// LegacyIters is the count used before the file format carried its own value. A
// file with a salt but no recorded count was written under it, so it must keep
// being read with it. Never use for new files.
const LegacyIters = 100_000

// SaltLen is the byte length of the per-file random salt.
const SaltLen = 16

// Cipher seals/opens secret values with AES-256-GCM.
type Cipher struct {
	aead cipher.AEAD
}

// NewSalt returns a fresh random salt to store alongside the ciphertext (in the
// config file's encryptionsalt field), so the same passphrase re-derives the
// same key on decrypt.
func NewSalt() ([]byte, error) {
	salt := make([]byte, SaltLen)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, fmt.Errorf("generate salt: %w", err)
	}
	return salt, nil
}

// NewCipherFromPassphrase derives a 32-byte AES key from passphrase+salt via
// PBKDF2-SHA256 with iters rounds, and builds the AES-256-GCM cipher. An empty
// passphrase is an error (a secret must never be "encrypted" under the empty
// key). iters must be the count the target file was written with — decrypting
// with a different count derives a different key and fails as if the passphrase
// were wrong.
func NewCipherFromPassphrase(passphrase string, salt []byte, iters int) (*Cipher, error) {
	if passphrase == "" {
		return nil, errors.New("passphrase is empty (set SKLP_SECRETS_PASSPHRASE)")
	}
	if len(salt) == 0 {
		return nil, errors.New("salt is empty")
	}
	if iters <= 0 {
		return nil, fmt.Errorf("pbkdf2 iterations must be positive, got %d", iters)
	}
	key, err := pbkdf2.Key(sha256.New, passphrase, salt, iters, 32)
	if err != nil {
		return nil, fmt.Errorf("derive key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("init aes: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("init gcm: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// Encrypt seals plaintext. Output layout: nonce || ciphertext so a single field
// stores the blob without a separate nonce column. (Copied from core.)
func (c *Cipher) Encrypt(plaintext string) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	ciphertext := c.aead.Seal(nil, nonce, []byte(plaintext), nil)
	return append(nonce, ciphertext...), nil
}

// Decrypt opens a blob produced by Encrypt. A too-short blob, a corrupt
// nonce/tag, or a wrong passphrase all return an error (indistinguishable from
// the AEAD side — treat any error as "secret unavailable / wrong passphrase").
// (Copied from core.)
func (c *Cipher) Decrypt(blob []byte) (string, error) {
	ns := c.aead.NonceSize()
	if len(blob) < ns {
		return "", errors.New("ciphertext too short")
	}
	nonce, ciphertext := blob[:ns], blob[ns:]
	plaintext, err := c.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt (wrong passphrase?): %w", err)
	}
	return string(plaintext), nil
}
