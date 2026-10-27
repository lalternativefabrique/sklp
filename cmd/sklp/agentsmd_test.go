package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSpliceSectionKeepsHandWrittenText(t *testing.T) {
	first := spliceSection([]byte("# Repo\n\nhand notes\n"), "v1\n")
	if !strings.HasPrefix(string(first), "# Repo\n\nhand notes\n\n"+agentsMDBegin) {
		t.Fatalf("section not appended after the notes:\n%s", first)
	}
	second := spliceSection(append(first, []byte("\nmore notes\n")...), "v2\n")
	got := string(second)
	if strings.Contains(got, "v1") || !strings.Contains(got, "v2") {
		t.Fatalf("section not replaced:\n%s", got)
	}
	if strings.Count(got, agentsMDBegin) != 1 || !strings.Contains(got, "hand notes") || !strings.Contains(got, "more notes") {
		t.Fatalf("hand-written text lost or section duplicated:\n%s", got)
	}
}

func TestAgentsMDNamesReconciledSpacesAndWatches(t *testing.T) {
	repo := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".sklp/deploy-root.yaml", "kind: deploy-root\nnode: n1\ndeployments:\n  - space: app-production\n    path: .sklp/prod/deploy.yaml\n")
	write(".sklp/image-watches.yaml", "watches:\n  - name: app-core\n    service: core\n    space: app-production\n    image_repo: reg/app/core\n")
	write("CLAUDE.md", "# Rules\n")

	if rc := runAgentsMD(repo, false); rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	md, _ := os.ReadFile(filepath.Join(repo, "AGENTS.md"))
	for _, want := range []string{"`app-production`", "`.sklp/prod/deploy.yaml`", "`n1`", "`reg/app/core`", "Never `sklp deploy apply`"} {
		if !strings.Contains(string(md), want) {
			t.Errorf("AGENTS.md lacks %s", want)
		}
	}
	claude, _ := os.ReadFile(filepath.Join(repo, "CLAUDE.md"))
	if strings.Count(string(claude), claudeImport) != 1 {
		t.Fatalf("CLAUDE.md import:\n%s", claude)
	}
	if rc := runAgentsMD(repo, true); rc != 0 {
		t.Fatal("a fresh AGENTS.md must pass --check")
	}
	if rc := runAgentsMD(repo, false); rc != 0 {
		t.Fatal(rc)
	}
	claude, _ = os.ReadFile(filepath.Join(repo, "CLAUDE.md"))
	if strings.Count(string(claude), claudeImport) != 1 {
		t.Fatal("a second run must not append the import again")
	}
}
