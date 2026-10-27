package stackconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/internal/secretcrypto"
)

// A new file must record the iteration count it sealed with, so the count can
// be raised later without stranding it.
func TestNewFileRecordsItsIterationCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")

	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetSecret("SCW_SECRET_KEY", "value", "passphrase"); err != nil {
		t.Fatal(err)
	}
	if c.Iters != secretcrypto.DefaultIters {
		t.Fatalf("Iters = %d, want DefaultIters %d", c.Iters, secretcrypto.DefaultIters)
	}
	if err := Save(path, c); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "encryptioniters:") {
		t.Fatalf("saved file does not persist the iteration count:\n%s", raw)
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reloaded.Resolve("passphrase")
	if err != nil {
		t.Fatalf("reload+resolve: %v", err)
	}
	if got["SCW_SECRET_KEY"] != "value" {
		t.Fatalf("resolved = %q, want value", got["SCW_SECRET_KEY"])
	}
}

// A file written before the field existed has a salt but no count. It must keep
// decrypting under the legacy count — this is the regression that would silently
// lock an operator out of their own secrets.
func TestLegacyFileWithoutIterationCountStillDecrypts(t *testing.T) {
	dir := t.TempDir()
	legacyPath := filepath.Join(dir, "legacy.yaml")

	// Build a legacy file the way the old code did: seal at LegacyIters and
	// write the salt WITHOUT an encryptioniters field.
	seed, err := Load(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.SetSecret("TOKEN", "legacy-value", "passphrase"); err != nil {
		t.Fatal(err)
	}
	if seed.Iters != secretcrypto.DefaultIters {
		t.Fatalf("precondition: fresh file should seal at DefaultIters, got %d", seed.Iters)
	}
	// Re-seal at the legacy count and drop the recorded field.
	legacy := &Config{Salt: seed.Salt, Plain: map[string]string{}, Secret: map[string]string{}}
	if legacy.KDFIters() != secretcrypto.LegacyIters {
		t.Fatalf("a file with no recorded count must read as legacy, got %d", legacy.KDFIters())
	}
	if err := legacy.SetSecret("TOKEN", "legacy-value", "passphrase"); err != nil {
		t.Fatal(err)
	}
	if err := Save(legacyPath, legacy); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "encryptioniters:") {
		t.Fatalf("legacy fixture must not record a count:\n%s", raw)
	}

	reloaded, err := Load(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reloaded.Resolve("passphrase")
	if err != nil {
		t.Fatalf("a legacy file must stay decryptable: %v", err)
	}
	if got["TOKEN"] != "legacy-value" {
		t.Fatalf("resolved = %q, want legacy-value", got["TOKEN"])
	}
}

func TestWrongPassphraseNamesTheOffendingKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	c, _ := Load(path)
	if err := c.SetSecret("API_KEY", "value", "right"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Resolve("wrong"); err == nil {
		t.Fatal("resolve with a wrong passphrase must fail")
	} else if !strings.Contains(err.Error(), "API_KEY") {
		t.Fatalf("error should name the offending key, got: %v", err)
	}
}
