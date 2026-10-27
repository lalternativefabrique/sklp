package secretcrypto

import (
	"bytes"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	salt, err := NewSalt()
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewCipherFromPassphrase("correct horse battery staple", salt, LegacyIters)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := c.Encrypt("s3cr3t-value")
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Decrypt(blob)
	if err != nil {
		t.Fatal(err)
	}
	if got != "s3cr3t-value" {
		t.Fatalf("decrypt = %q, want s3cr3t-value", got)
	}
}

// TestWrongPassphraseFails: the same salt but a different passphrase must NOT
// decrypt — this is what makes a mistyped SKLP_SECRETS_PASSPHRASE fail loudly
// instead of silently returning garbage.
func TestWrongPassphraseFails(t *testing.T) {
	salt, _ := NewSalt()
	enc, _ := NewCipherFromPassphrase("right", salt, LegacyIters)
	blob, _ := enc.Encrypt("value")

	dec, _ := NewCipherFromPassphrase("wrong", salt, LegacyIters) // same salt, wrong passphrase
	if _, err := dec.Decrypt(blob); err == nil {
		t.Fatal("decrypt with wrong passphrase should fail, got nil error")
	}
}

// TestSaltIsRandom: two NewSalt calls differ (so two files get distinct keys
// from the same passphrase).
func TestSaltIsRandom(t *testing.T) {
	a, _ := NewSalt()
	b, _ := NewSalt()
	if bytes.Equal(a, b) {
		t.Fatal("two salts are identical")
	}
	if len(a) != SaltLen {
		t.Fatalf("salt len = %d, want %d", len(a), SaltLen)
	}
}

func TestEmptyPassphraseRejected(t *testing.T) {
	salt, _ := NewSalt()
	if _, err := NewCipherFromPassphrase("", salt, LegacyIters); err == nil {
		t.Fatal("empty passphrase should be rejected")
	}
}
