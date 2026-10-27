package spec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeBuiltinCase(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secops.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExtendsBuiltinSecops(t *testing.T) {
	path := writeBuiltinCase(t, "version: 1\nkind: pipeline\nextends: sklp:secops\ntools:\n  go: \"1.25.13\"\n")
	p, err := LoadPipeline(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "secops" || len(p.Steps) != 4 {
		t.Fatalf("got name=%q steps=%d", p.Name, len(p.Steps))
	}
	if p.Tools["go"] != "1.25.13" || p.Tools["trivy"] == "" {
		t.Fatalf("tools not merged: %v", p.Tools)
	}
}

func TestExtendsUnknownBuiltin(t *testing.T) {
	path := writeBuiltinCase(t, "version: 1\nkind: pipeline\nextends: sklp:nope\n")
	_, err := LoadPipeline(path)
	if err == nil || !strings.Contains(err.Error(), "secops") {
		t.Fatalf("want error listing available built-ins, got %v", err)
	}
}
