package stackconfig

import (
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/internal/secretcrypto"
)

func TestRotateResealsUnderTheNewPassphrase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	c.SetPlain("PORT", "4100")
	if err := c.SetSecret("SCW_SECRET_KEY", "scw-value", "old-pass"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetSecret("S3_SECRET_KEY", "s3-value", "old-pass"); err != nil {
		t.Fatal(err)
	}

	rotated, err := c.Rotate("old-pass", "new-pass")
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}

	// The new passphrase opens everything.
	got, err := rotated.Resolve("new-pass")
	if err != nil {
		t.Fatalf("resolve with the new passphrase: %v", err)
	}
	if got["SCW_SECRET_KEY"] != "scw-value" || got["S3_SECRET_KEY"] != "s3-value" {
		t.Fatalf("values did not survive rotation: %v", got)
	}
	if got["PORT"] != "4100" {
		t.Fatalf("plain value lost: %v", got)
	}

	// The old one no longer does — otherwise rotating would be cosmetic.
	if _, err := rotated.Resolve("old-pass"); err == nil {
		t.Fatal("the old passphrase still decrypts after rotation")
	}
}

// The salt must be redrawn. Keeping it would change the passphrase while
// leaving half the key material intact, and is the trap SetSecret falls into
// (ensureSalt is a no-op once a salt exists).
func TestRotateDrawsAFreshSalt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	c, _ := Load(path)
	if err := c.SetSecret("TOKEN", "value", "old-pass"); err != nil {
		t.Fatal(err)
	}
	oldSalt := c.Salt

	rotated, err := c.Rotate("old-pass", "new-pass")
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Salt == oldSalt {
		t.Fatal("rotation reused the old salt")
	}
	if rotated.Salt == "" {
		t.Fatal("rotation produced no salt")
	}
}

// Rotating is also the upgrade path for a file written at the old KDF cost.
func TestRotateUpgradesLegacyIterations(t *testing.T) {
	// Build the pre-field shape for real: a salt, no recorded count, and a
	// ciphertext actually sealed at LegacyIters. Sealing at DefaultIters and
	// then zeroing Iters would describe a file that never existed.
	salt, err := secretcrypto.NewSalt()
	if err != nil {
		t.Fatal(err)
	}
	legacy := &Config{
		Salt:   base64.StdEncoding.EncodeToString(salt),
		Plain:  map[string]string{},
		Secret: map[string]string{},
	}
	if legacy.KDFIters() != secretcrypto.LegacyIters {
		t.Fatalf("precondition: want a legacy-count config, got %d", legacy.KDFIters())
	}
	if err := legacy.SetSecret("TOKEN", "value", "old-pass"); err != nil {
		t.Fatal(err)
	}
	if legacy.Iters != 0 {
		t.Fatalf("precondition: a legacy file records no count, got %d", legacy.Iters)
	}

	rotated, err := legacy.Rotate("old-pass", "new-pass")
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Iters != secretcrypto.DefaultIters {
		t.Fatalf("rotated Iters = %d, want DefaultIters %d", rotated.Iters, secretcrypto.DefaultIters)
	}
	got, err := rotated.Resolve("new-pass")
	if err != nil {
		t.Fatalf("resolve after upgrade: %v", err)
	}
	if got["TOKEN"] != "value" {
		t.Fatalf("value lost in the KDF upgrade: %v", got)
	}
}

// A wrong current passphrase must abort before anything is rewritten.
func TestRotateWithWrongCurrentPassphraseFails(t *testing.T) {
	c, _ := Load(filepath.Join(t.TempDir(), "config.yaml"))
	if err := c.SetSecret("TOKEN", "value", "right"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Rotate("wrong", "new-pass"); err == nil {
		t.Fatal("rotate accepted a wrong current passphrase")
	}
}

func TestRotateRejectsEmptyOrIdenticalNewPassphrase(t *testing.T) {
	c, _ := Load(filepath.Join(t.TempDir(), "config.yaml"))
	if err := c.SetSecret("TOKEN", "value", "same"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Rotate("same", ""); err == nil {
		t.Fatal("rotate accepted an empty new passphrase")
	}
	if _, err := c.Rotate("same", "same"); err == nil {
		t.Fatal("rotate accepted an unchanged passphrase")
	} else if !strings.Contains(err.Error(), "identical") {
		t.Fatalf("error should say the passphrase is unchanged, got: %v", err)
	}
}

// The receiver is left alone, so a failed Save cannot leave a half-rotated
// config behind in memory.
func TestRotateDoesNotMutateTheReceiver(t *testing.T) {
	c, _ := Load(filepath.Join(t.TempDir(), "config.yaml"))
	if err := c.SetSecret("TOKEN", "value", "old-pass"); err != nil {
		t.Fatal(err)
	}
	beforeSalt, beforeCT := c.Salt, c.Secret["TOKEN"]

	if _, err := c.Rotate("old-pass", "new-pass"); err != nil {
		t.Fatal(err)
	}
	if c.Salt != beforeSalt || c.Secret["TOKEN"] != beforeCT {
		t.Fatal("Rotate mutated the receiver")
	}
	if got, err := c.Resolve("old-pass"); err != nil || got["TOKEN"] != "value" {
		t.Fatalf("receiver no longer opens with its own passphrase: %v %v", got, err)
	}
}
