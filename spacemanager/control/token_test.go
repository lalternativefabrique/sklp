package control

import (
	"os"
	"testing"
)

// TestEnsureTokenIdempotentAndPerms: first call generates+persists a token 0600,
// subsequent calls return the SAME token, and LoadToken reads it back.
func TestEnsureTokenIdempotentAndPerms(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	tok1, err := EnsureToken("production")
	if err != nil {
		t.Fatalf("EnsureToken: %v", err)
	}
	if tok1 == "" {
		t.Fatal("empty token")
	}

	// File perms must be owner-only (it is a secret).
	info, err := os.Stat(TokenPath("production"))
	if err != nil {
		t.Fatalf("stat token: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("token perms = %o, want 600", perm)
	}

	tok2, err := EnsureToken("production")
	if err != nil {
		t.Fatalf("EnsureToken (2nd): %v", err)
	}
	if tok1 != tok2 {
		t.Fatalf("token not stable: %q != %q", tok1, tok2)
	}

	loaded, err := LoadToken("production")
	if err != nil {
		t.Fatalf("LoadToken: %v", err)
	}
	if loaded != tok1 {
		t.Fatalf("LoadToken = %q, want %q", loaded, tok1)
	}
}

// TestLoadTokenMissing: a space that never opened a listener has no token.
func TestLoadTokenMissing(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, err := LoadToken("ghost"); !os.IsNotExist(err) {
		t.Fatalf("want os.IsNotExist, got %v", err)
	}
}
