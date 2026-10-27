package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/internal/skillgen"
)

func TestParseTargets(t *testing.T) {
	cases := []struct {
		in      string
		want    []skillgen.Target
		wantErr bool
	}{
		{"claude,cursor", []skillgen.Target{skillgen.TargetClaude, skillgen.TargetCursor}, false},
		{"cursor", []skillgen.Target{skillgen.TargetCursor}, false},
		{"  Claude , CURSOR ", []skillgen.Target{skillgen.TargetClaude, skillgen.TargetCursor}, false},
		{"claude,claude", []skillgen.Target{skillgen.TargetClaude}, false}, // de-dup
		{"codex,vibe,zed,cline", []skillgen.Target{skillgen.TargetCodex, skillgen.TargetVibe, skillgen.TargetZed, skillgen.TargetCline}, false},
		{"all", skillgen.AllTargets, false},
		{"claude,all", skillgen.AllTargets, false}, // "all" absorbs the explicit one
		{"", nil, true},
		{"vscode", nil, true},
	}
	for _, c := range cases {
		got, err := parseTargets(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseTargets(%q): expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseTargets(%q): unexpected error %v", c.in, err)
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("parseTargets(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("parseTargets(%q)[%d] = %v, want %v", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestPrintSkillWritesNothing(t *testing.T) {
	repo := t.TempDir()
	var out bytes.Buffer
	if err := printSkill(&out, repo, "sklp-operator", skillgen.TargetClaude); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "name: sklp-operator") {
		t.Fatalf("unexpected output: %q", out.String())
	}
	entries, _ := os.ReadDir(repo)
	if len(entries) != 0 {
		t.Fatalf("printSkill wrote into the repo: %v", entries)
	}
	if err := printSkill(&out, repo, "nope", skillgen.TargetClaude); err == nil {
		t.Fatal("want error for unknown skill")
	}
}
