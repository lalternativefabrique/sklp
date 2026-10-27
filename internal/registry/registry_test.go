package registry

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func withTempConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv(EnvFile, "")
	return filepath.Join(dir, "sklp", "registries.yaml")
}

func TestLoadMissingFileIsNotAnError(t *testing.T) {
	want := withTempConfig(t)
	f, path, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if len(f.Registries) != 0 {
		t.Errorf("expected no credentials, got %v", f.Registries)
	}
}

func TestSaveRoundTripIs0600(t *testing.T) {
	path := withTempConfig(t)
	f := &File{}
	f.Set("rg.fr-par.scw.cloud", Credential{Username: "nologin", Auth: EncodeAuth("nologin", "s3cr3t")})
	if err := f.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode = %#o, want 0600", mode)
	}

	back, _, err := Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, err := back.Resolve("rg.fr-par.scw.cloud")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Username != "nologin" || got.Password != "s3cr3t" {
		t.Errorf("resolved %+v, want nologin/s3cr3t", got)
	}
}

func TestResolveUnknownHostReportsErrNoCredential(t *testing.T) {
	withTempConfig(t)
	_, err := Resolve("ghcr.io")
	if !errors.Is(err, ErrNoCredential) {
		t.Fatalf("err = %v, want ErrNoCredential", err)
	}
}

func TestResolveRunsCredentialCommand(t *testing.T) {
	f := &File{Registries: map[string]Credential{
		"ghcr.io": {Username: "oauth2", CredentialCommand: "printf ghp_from_store"},
	}}
	got, err := f.Resolve("ghcr.io")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Password != "ghp_from_store" {
		t.Errorf("password = %q, want ghp_from_store", got.Password)
	}
}

// A command that fails must surface as an error rather than as an empty
// password, which would reach the registry as an anonymous push and be
// refused with a message naming neither this file nor the command.
func TestResolveFailingCredentialCommandErrors(t *testing.T) {
	f := &File{Registries: map[string]Credential{
		"ghcr.io": {Username: "oauth2", CredentialCommand: "exit 7"},
	}}
	if _, err := f.Resolve("ghcr.io"); err == nil {
		t.Fatal("expected an error for a failing credential_command")
	}
}

func TestResolveEmptyCredentialCommandOutputErrors(t *testing.T) {
	f := &File{Registries: map[string]Credential{
		"ghcr.io": {Username: "oauth2", CredentialCommand: "true"},
	}}
	if _, err := f.Resolve("ghcr.io"); err == nil {
		t.Fatal("expected an error when the command prints nothing")
	}
}

func TestDeleteReportsWhetherHostWasPresent(t *testing.T) {
	f := &File{}
	f.Set("ghcr.io", Credential{Username: "oauth2", Auth: EncodeAuth("oauth2", "t")})
	if !f.Delete("ghcr.io") {
		t.Error("Delete on a present host = false, want true")
	}
	if f.Delete("ghcr.io") {
		t.Error("Delete on an absent host = true, want false")
	}
}

func TestRememberedWinsForThisProcess(t *testing.T) {
	t.Cleanup(ForgetAll)
	ForgetAll()
	if _, ok := Remembered("ghcr.io"); ok {
		t.Fatal("nothing should be remembered before Remember")
	}
	Remember("ghcr.io", "oauth2", "tok")
	got, ok := Remembered("ghcr.io")
	if !ok || got.Password != "tok" {
		t.Errorf("Remembered = %+v, %v", got, ok)
	}
}
