//go:build linux

package imagebuild

import (
	"os"
	"path/filepath"
	"testing"

	parse "github.com/lalternativefabrique/sklp/pipeline/imagebuild"
)

// TestCopyInto_RespectsDockerignore: a COPY . /app from a host context with a
// .dockerignore must skip .git / node_modules / .env while copying the source.
func TestCopyInto_RespectsDockerignore(t *testing.T) {
	ctx := t.TempDir()
	// Build a fake context tree.
	write := func(rel, data string) {
		p := filepath.Join(ctx, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("main.go", "package main")
	write(".env", "SECRET=hunter2")
	write(".git/config", "[core]")
	write("node_modules/dep/index.js", "x")
	write("src/util.go", "package util")

	ig := parse.ParseDockerignore(".git\nnode_modules\n.env\n")

	dst := t.TempDir()
	if err := copyInto(ctx, dst, "/", ctx, ig); err != nil {
		t.Fatalf("copyInto: %v", err)
	}

	// Present: source files.
	for _, keep := range []string{"main.go", "src/util.go"} {
		if _, err := os.Stat(filepath.Join(dst, keep)); err != nil {
			t.Errorf("%s should have been copied: %v", keep, err)
		}
	}
	// Absent: ignored paths (the leak vector).
	for _, gone := range []string{".env", ".git", ".git/config", "node_modules", "node_modules/dep/index.js"} {
		if _, err := os.Stat(filepath.Join(dst, gone)); err == nil {
			t.Errorf("%s LEAKED into the image despite .dockerignore", gone)
		}
	}
}

// TestDoCopy_ExpandsGlob: a COPY source with a wildcard (Docker semantics) must
// expand against the context, not be stat'd literally. Regression for the amv
// Next.js image whose `COPY package.json pnpm-lock.yaml* .npmrc ./` failed with
// `stat .../pnpm-lock.yaml*: no such file or directory`.
func TestDoCopy_ExpandsGlob(t *testing.T) {
	ctx := t.TempDir()
	for _, f := range []string{"package.json", "pnpm-lock.yaml", ".npmrc"} {
		if err := os.WriteFile(filepath.Join(ctx, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rootfs := t.TempDir()

	in := parse.Instruction{Args: "package.json pnpm-lock.yaml* .npmrc ./"}
	if err := doCopy(&parse.Dockerfile{}, nil, 0, rootfs, in, Options{ContextDir: ctx}, "/app"); err != nil {
		t.Fatalf("doCopy with glob: %v", err)
	}
	// All three land under the WORKDIR-relative dest /app/.
	for _, want := range []string{"package.json", "pnpm-lock.yaml", ".npmrc"} {
		if _, err := os.Stat(filepath.Join(rootfs, "app", want)); err != nil {
			t.Errorf("%s not copied (glob expansion failed): %v", want, err)
		}
	}
}
