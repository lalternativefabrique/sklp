package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseDotenvValues_MissingFileIsEmpty(t *testing.T) {
	dir := t.TempDir()
	got, err := parseDotenvValues(filepath.Join(dir, "nope"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty map, got %v", got)
	}
}

func TestParseDotenvValues_SkipsCommentsAndBlankLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env.secret")
	if err := os.WriteFile(path, []byte("# comment\n\nA=1\nB=\"quoted value\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := parseDotenvValues(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got["A"] != "1" || got["B"] != "quoted value" {
		t.Fatalf("unexpected result: %v", got)
	}
}

func TestReadDotenvLines_PreservesCommentsAndOrder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env.secret")
	content := "# header\nA=1\n\nB=2\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	lines, err := readDotenvLines(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines, got %d: %+v", len(lines), lines)
	}
	if lines[0].Key != "" || lines[0].Raw != "# header" {
		t.Fatalf("line 0 not preserved: %+v", lines[0])
	}
	if lines[1].Key != "A" || lines[1].Value != "1" {
		t.Fatalf("line 1 not parsed: %+v", lines[1])
	}
}

func TestMergeDotenvLines_UpdatesAndAppends(t *testing.T) {
	lines := []dotenvLine{
		{Raw: "# comment", Key: ""},
		{Raw: "A=old", Key: "A", Value: "old"},
		{Raw: "LOCAL=keep", Key: "LOCAL", Value: "keep"},
	}
	updated, added := mergeDotenvLines(&lines, map[string]string{"A": "new", "B": "brand-new"})
	if updated != 1 || added != 1 {
		t.Fatalf("updated=%d added=%d, want 1,1", updated, added)
	}
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines after merge, got %d: %+v", len(lines), lines)
	}
	foundA, foundB, foundLocal := false, false, false
	for _, l := range lines {
		switch l.Key {
		case "A":
			foundA = l.Value == "new"
		case "B":
			foundB = l.Value == "brand-new"
		case "LOCAL":
			foundLocal = l.Value == "keep"
		}
	}
	if !foundA || !foundB || !foundLocal {
		t.Fatalf("merge result incorrect: %+v", lines)
	}
}

func TestMergeDotenvLines_NoOpWhenValueUnchanged(t *testing.T) {
	lines := []dotenvLine{{Raw: "A=same", Key: "A", Value: "same"}}
	updated, added := mergeDotenvLines(&lines, map[string]string{"A": "same"})
	if updated != 0 || added != 0 {
		t.Fatalf("expected no-op, got updated=%d added=%d", updated, added)
	}
}

func TestWriteDotenvLines_AtomicWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env.secret")
	lines := []dotenvLine{{Raw: "A=1", Key: "A", Value: "1"}}
	if err := writeDotenvLines(path, lines); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "A=1\n" {
		t.Fatalf("unexpected content: %q", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != ".env.secret" {
			t.Fatalf("leftover temp file: %s", e.Name())
		}
	}
}

func TestQuoteDotenvValue_QuotesOnlyWhenNeeded(t *testing.T) {
	if quoteDotenvValue("simple") != "simple" {
		t.Fatal("simple value should not be quoted")
	}
	if quoteDotenvValue("has space") != `"has space"` {
		t.Fatalf("got %q", quoteDotenvValue("has space"))
	}
}
