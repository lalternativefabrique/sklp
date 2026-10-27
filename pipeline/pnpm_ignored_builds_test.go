package pipeline

import (
	"strings"
	"testing"
)

func TestParseIgnoredBuildPackages(t *testing.T) {
	tail := `Lockfile is up to date, resolution step is skipped
Already up to date

 ╭ Warning ─────────────────────────────────────────────────────────╮
 │   Ignored build scripts: @tailwindcss/oxide@4.1.7, esbuild@0.25.4, │
 │   sharp@0.34.5.                                                    │
 ╰───────────────────────────────────────────────────────────────────╯
Run "pnpm approve-builds" to pick which dependencies should be allowed.`

	got := parseIgnoredBuildPackages(tail)
	// Only the first line after the marker is parsed; the wrapped
	// "sharp" lives on the next line, so we expect the two on the marker
	// line. (The hint still guides the user; full extraction across
	// pnpm's box-wrapping is best-effort.)
	if len(got) < 2 {
		t.Fatalf("expected at least 2 packages, got %v", got)
	}
	if got[0] != "@tailwindcss/oxide" {
		t.Errorf("expected scoped name preserved, got %q", got[0])
	}
	for _, p := range got {
		if strings.ContainsAny(p, "0123456789") && strings.Contains(p, "@") && !strings.HasPrefix(p, "@") {
			t.Errorf("version suffix not stripped: %q", p)
		}
	}
}

func TestPnpmIgnoredBuildsHint(t *testing.T) {
	t.Run("emits actionable snippet on ignored-builds failure", func(t *testing.T) {
		tail := "Ignored build scripts: esbuild@0.25.4, sharp@0.34.5.\nRun \"pnpm approve-builds\""
		hint := pnpmIgnoredBuildsHint(tail)
		if hint == "" {
			t.Fatal("expected a hint, got empty")
		}
		for _, want := range []string{"allowBuilds", "esbuild", "sharp", "pnpm-workspace.yaml"} {
			if !strings.Contains(hint, want) {
				t.Errorf("hint missing %q:\n%s", want, hint)
			}
		}
	})

	t.Run("silent on unrelated failures", func(t *testing.T) {
		if h := pnpmIgnoredBuildsHint("npm ERR! code ELIFECYCLE\nsome other error"); h != "" {
			t.Errorf("expected no hint for unrelated failure, got:\n%s", h)
		}
	})
}
