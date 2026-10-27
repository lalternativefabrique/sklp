package repofacts

import (
	"os"
	"path/filepath"
	"testing"
)

// writeRepo materialises a tiny repo tree under t.TempDir and returns
// (root, sorted file list) the way Scan's walker would.
func writeRepo(t *testing.T, files map[string]string) (string, []string) {
	t.Helper()
	root := t.TempDir()
	var list []string
	for rel, content := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		list = append(list, p)
	}
	return root, list
}

func TestDetectNode_PMVersionFromCorepack(t *testing.T) {
	root, files := writeRepo(t, map[string]string{
		"package.json":   `{"name":"x","packageManager":"pnpm@8.15.0+sha512.abc","engines":{"node":"20.11.0"}}`,
		"pnpm-lock.yaml": "lockfileVersion: '6.0'\n",
	})
	lf := detectNode(files, root)
	if lf == nil {
		t.Fatal("expected node facts")
	}
	if lf.PackageManager != "pnpm" {
		t.Errorf("pm = %q", lf.PackageManager)
	}
	if lf.PackageManagerVersion != "8.15.0" { // +sha stripped
		t.Errorf("pm version = %q, want 8.15.0", lf.PackageManagerVersion)
	}
	if lf.ToolchainVersion != "20.11.0" {
		t.Errorf("node version = %q", lf.ToolchainVersion)
	}
}

func TestDetectNode_VersionFromSubdirNvmrc(t *testing.T) {
	// Monorepo: lockfile at root, but the node version is pinned in
	// frontend/.nvmrc — the detector must look beyond the root.
	root, files := writeRepo(t, map[string]string{
		"pnpm-lock.yaml":        "lockfileVersion: '9.0'\n",
		"package.json":          `{"name":"root"}`,
		"frontend/.nvmrc":       "18.20.8\n",
		"frontend/package.json": `{"name":"fe"}`,
	})
	lf := detectNode(files, root)
	if lf == nil {
		t.Fatal("expected node facts")
	}
	if lf.ToolchainVersion != "18.20.8" {
		t.Errorf("node version = %q, want 18.20.8 (from frontend/.nvmrc)", lf.ToolchainVersion)
	}
}
