package pipeline

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeEnvFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func TestLoadPipelineEnvFiles_Basic(t *testing.T) {
	dir := t.TempDir()
	writeEnvFile(t, dir, ".env", "FOO=bar\nBAZ=qux\n")

	t.Setenv("FOO", "")
	t.Setenv("BAZ", "")
	os.Unsetenv("FOO")
	os.Unsetenv("BAZ")

	var buf bytes.Buffer
	applied, err := LoadPipelineEnvFiles([]string{".env"}, dir, &buf)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if applied["FOO"] != "bar" || applied["BAZ"] != "qux" {
		t.Fatalf("applied=%v", applied)
	}
	if os.Getenv("FOO") != "bar" || os.Getenv("BAZ") != "qux" {
		t.Fatalf("env not set: FOO=%q BAZ=%q", os.Getenv("FOO"), os.Getenv("BAZ"))
	}
	if !strings.Contains(buf.String(), "loaded env from .env (2 variables)") {
		t.Fatalf("expected info log, got %q", buf.String())
	}
}

func TestLoadPipelineEnvFiles_CommentsBlanksQuotes(t *testing.T) {
	dir := t.TempDir()
	writeEnvFile(t, dir, ".env", `# comment

A=plain
B="double quoted"
C='single quoted'
# another
D=  spaced
`)
	os.Unsetenv("A")
	os.Unsetenv("B")
	os.Unsetenv("C")
	os.Unsetenv("D")

	if _, err := LoadPipelineEnvFiles([]string{".env"}, dir, nil); err != nil {
		t.Fatalf("load: %v", err)
	}
	cases := map[string]string{
		"A": "plain",
		"B": "double quoted",
		"C": "single quoted",
		"D": "spaced",
	}
	for k, want := range cases {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s: got %q want %q", k, got, want)
		}
	}
}

func TestLoadPipelineEnvFiles_MissingFileSilent(t *testing.T) {
	dir := t.TempDir()
	writeEnvFile(t, dir, ".env", "X=1\n")
	os.Unsetenv("X")

	if _, err := LoadPipelineEnvFiles([]string{".env", ".env.local"}, dir, nil); err != nil {
		t.Fatalf("missing file must be silent, got %v", err)
	}
	if os.Getenv("X") != "1" {
		t.Fatalf("X not loaded")
	}
}

func TestLoadPipelineEnvFiles_HostWins(t *testing.T) {
	dir := t.TempDir()
	writeEnvFile(t, dir, ".env", "HOSTVAR=fromfile\n")
	t.Setenv("HOSTVAR", "fromshell")

	resolved, err := LoadPipelineEnvFiles([]string{".env"}, dir, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if resolved["HOSTVAR"] != "fromshell" {
		t.Fatalf("resolved should reflect host value; got %v", resolved)
	}
	if os.Getenv("HOSTVAR") != "fromshell" {
		t.Fatalf("HOSTVAR overridden: %q", os.Getenv("HOSTVAR"))
	}
}

func TestLoadPipelineEnvFiles_LastWinsAcrossFiles(t *testing.T) {
	dir := t.TempDir()
	writeEnvFile(t, dir, ".env", "K=first\n")
	writeEnvFile(t, dir, ".env.local", "K=second\n")
	os.Unsetenv("K")

	if _, err := LoadPipelineEnvFiles([]string{".env", ".env.local"}, dir, nil); err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := os.Getenv("K"); got != "second" {
		t.Fatalf("last-wins broken: %q", got)
	}
}

func TestLoadPipelineEnvFiles_NoPaths(t *testing.T) {
	applied, err := LoadPipelineEnvFiles(nil, "", nil)
	if err != nil || applied != nil {
		t.Fatalf("nil paths: applied=%v err=%v", applied, err)
	}
}
