package secretcrypto

import "testing"

// The iteration count is part of the key derivation, so it must travel with the
// ciphertext. These tests pin why: a mismatched count is indistinguishable from
// a wrong passphrase, which is exactly how raising the cost would silently
// strand existing files if the count were compiled in.

func TestNewFilesUseTheOWASPFloor(t *testing.T) {
	if DefaultIters < 600_000 {
		t.Fatalf("DefaultIters = %d, want >= 600000 (OWASP floor for PBKDF2-SHA256)", DefaultIters)
	}
	if LegacyIters >= DefaultIters {
		t.Fatalf("LegacyIters (%d) must stay below DefaultIters (%d)", LegacyIters, DefaultIters)
	}
}

func TestDecryptWithWrongIterationCountFails(t *testing.T) {
	salt, _ := NewSalt()
	enc, err := NewCipherFromPassphrase("same-passphrase", salt, LegacyIters)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := enc.Encrypt("value")
	if err != nil {
		t.Fatal(err)
	}

	// Same passphrase, same salt, different count → different key.
	dec, err := NewCipherFromPassphrase("same-passphrase", salt, DefaultIters)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dec.Decrypt(blob); err == nil {
		t.Fatal("decrypting with a different iteration count must fail; if this ever passes, the count is not part of the key")
	}
}

func TestRoundTripAtDefaultIters(t *testing.T) {
	salt, _ := NewSalt()
	c, err := NewCipherFromPassphrase("passphrase", salt, DefaultIters)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := c.Encrypt("value")
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Decrypt(blob)
	if err != nil {
		t.Fatal(err)
	}
	if got != "value" {
		t.Fatalf("decrypt = %q, want value", got)
	}
}

func TestNonPositiveItersRejected(t *testing.T) {
	salt, _ := NewSalt()
	for _, n := range []int{0, -1} {
		if _, err := NewCipherFromPassphrase("passphrase", salt, n); err == nil {
			t.Errorf("iters=%d should be rejected", n)
		}
	}
}
