package stackconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetSaveLoadResolve(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	const pass = "test-passphrase"

	c := &Config{Plain: map[string]string{}, Secret: map[string]string{}}
	c.SetPlain("PORT", "4100")
	if err := c.SetSecret("SCW_SECRET_KEY", "super-secret", pass); err != nil {
		t.Fatal(err)
	}
	if err := Save(path, c); err != nil {
		t.Fatal(err)
	}

	// The saved file must carry the ciphertext, NOT the plaintext.
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "super-secret") {
		t.Fatal("plaintext secret leaked into the config file")
	}
	if !strings.Contains(string(raw), "secure:") {
		t.Fatal("secret not marked with secure:")
	}
	if !strings.Contains(string(raw), "encryptionsalt:") {
		t.Fatal("missing encryptionsalt")
	}

	// Reload + resolve with the right passphrase.
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsSecret("SCW_SECRET_KEY") || got.IsSecret("PORT") {
		t.Fatal("secret/plain classification lost on reload")
	}
	resolved, err := got.Resolve(pass)
	if err != nil {
		t.Fatal(err)
	}
	if resolved["PORT"] != "4100" || resolved["SCW_SECRET_KEY"] != "super-secret" {
		t.Fatalf("resolved = %v, want PORT=4100 SCW_SECRET_KEY=super-secret", resolved)
	}
}

func TestResolveWrongPassphrase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	c := &Config{Plain: map[string]string{}, Secret: map[string]string{}}
	if err := c.SetSecret("K", "v", "right"); err != nil {
		t.Fatal(err)
	}
	_ = Save(path, c)
	got, _ := Load(path)
	if _, err := got.Resolve("wrong"); err == nil {
		t.Fatal("resolve with wrong passphrase should fail")
	}
}

func TestPlainOnlyNeedsNoPassphrase(t *testing.T) {
	c := &Config{Plain: map[string]string{"A": "1"}, Secret: map[string]string{}}
	got, err := c.Resolve("")
	if err != nil {
		t.Fatal(err)
	}
	if got["A"] != "1" {
		t.Fatalf("got %v", got)
	}
}

func TestSecretOverwritesPlainAndViceVersa(t *testing.T) {
	c := &Config{Plain: map[string]string{}, Secret: map[string]string{}}
	c.SetPlain("K", "plain")
	if err := c.SetSecret("K", "secret", "p"); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Plain["K"]; ok {
		t.Fatal("SetSecret should remove the plain entry")
	}
	c.SetPlain("K", "plain-again")
	if _, ok := c.Secret["K"]; ok {
		t.Fatal("SetPlain should remove the secret entry")
	}
}

func TestLoadMissingFile(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Keys()) != 0 {
		t.Fatal("missing file should yield empty config")
	}
}

func TestSaveSplitsSecretsFromConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	c := &Config{Plain: map[string]string{}, Secret: map[string]string{}}
	c.SetPlain("PORT", "4100")
	if err := c.SetSecret("SCW_SECRET_KEY", "super-secret", "pass"); err != nil {
		t.Fatal(err)
	}
	if err := Save(path, c); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	text := string(raw)
	cfg, sec := strings.Index(text, "config:"), strings.Index(text, "secrets:")
	if cfg < 0 || sec < 0 {
		t.Fatalf("expected both sections, got:\n%s", text)
	}
	key := strings.Index(text, "SCW_SECRET_KEY")
	if key < sec {
		t.Fatalf("sealed entry written under config, want secrets:\n%s", text)
	}
	if strings.Index(text, "PORT") > sec {
		t.Fatalf("plain entry written under secrets:\n%s", text)
	}
}

func TestLoadReadsSealedEntriesLeftUnderConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	legacy := "encryptionsalt: c2FsdA==\nencryptioniters: 600000\nconfig:\n    PORT: \"4100\"\n    SCW_SECRET_KEY:\n        secure: Y2lwaGVy\n"
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsSecret("SCW_SECRET_KEY") || got.IsSecret("PORT") {
		t.Fatalf("legacy layout misread: secret=%v plain=%v", got.Secret, got.Plain)
	}
}
