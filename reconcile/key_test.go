package reconcile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSealerKeepsItsKeyAcrossRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sklp", KeyName)
	first, err := LoadSealer(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file %v, mode %v: want 0600", err, info.Mode().Perm())
	}
	sealed, err := first.Encrypt("node-token")
	if err != nil {
		t.Fatal(err)
	}
	again, err := LoadSealer(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := again.Decrypt(sealed); err != nil || got != "node-token" {
		t.Fatalf("a restart must open what the previous run sealed: %q, %v", got, err)
	}
}
