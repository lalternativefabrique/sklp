package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunCompletion verifies the per-shell script dispatch, error
// handling for unknown shells, and the usage banner when no shell is
// supplied. We don't try to execute the scripts in a real shell here;
// that's covered by the manual smoke test documented in the PR.
func TestRunCompletion(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantExit int
		wantOut  string // substring expected on stdout
		wantErr  string // substring expected on stderr
	}{
		{
			name:     "bash emits _sklp function",
			args:     []string{"bash"},
			wantExit: 0,
			wantOut:  "_sklp()",
		},
		{
			name:     "bash registers the completer",
			args:     []string{"bash"},
			wantExit: 0,
			wantOut:  "complete -F _sklp sklp",
		},
		{
			name:     "bash shells back into the __complete helper",
			args:     []string{"bash"},
			wantExit: 0,
			wantOut:  `sklp __complete "$dyn"`,
		},
		{
			name:     "bash completes a declared enum flag",
			args:     []string{"bash"},
			wantExit: 0,
			wantOut:  "backlog in_progress done wontfix",
		},
		{
			name:     "zsh has compdef header",
			args:     []string{"zsh"},
			wantExit: 0,
			wantOut:  "#compdef sklp",
		},
		{
			name:     "zsh defines _sklp function",
			args:     []string{"zsh"},
			wantExit: 0,
			wantOut:  "_sklp()",
		},
		{
			name:     "fish uses complete -c sklp",
			args:     []string{"fish"},
			wantExit: 0,
			wantOut:  "complete -c sklp",
		},
		{
			name:     "help on stdout",
			args:     []string{"--help"},
			wantExit: 0,
			wantOut:  "Usage: sklp completion",
		},
		{
			name:     "missing arg prints usage to stderr",
			args:     []string{},
			wantExit: 2,
			wantErr:  "Usage: sklp completion",
		},
		{
			name:     "unknown shell errors out",
			args:     []string{"powershell"},
			wantExit: 2,
			wantErr:  "unsupported shell",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			got := runCompletionTo(&out, &errb, tc.args)
			if got != tc.wantExit {
				t.Fatalf("exit = %d, want %d (stderr=%q)", got, tc.wantExit, errb.String())
			}
			if tc.wantOut != "" && !strings.Contains(out.String(), tc.wantOut) {
				t.Fatalf("stdout missing %q\nfull stdout:\n%s", tc.wantOut, out.String())
			}
			if tc.wantErr != "" && !strings.Contains(errb.String(), tc.wantErr) {
				t.Fatalf("stderr missing %q\nfull stderr:\n%s", tc.wantErr, errb.String())
			}
		})
	}
}

// TestCompletionReflectsTree is the regression guard for the exact drift this
// feature kills: the generated completion (built from rootCommand) must mention
// `space` AND its sub-verbs, and `deploy` with its sub-verbs — none of which the
// old hand-written scripts carried. Asserted across all three shells.
func TestCompletionReflectsTree(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish"} {
		var out, errb bytes.Buffer
		if code := runCompletionTo(&out, &errb, []string{shell}); code != 0 {
			t.Fatalf("%s: exit %d (stderr=%q)", shell, code, errb.String())
		}
		s := out.String()
		wants := []string{
			// space verb + its full sub-tree.
			"space", "up", "ls", "get", "down", "exec", "purge",
			// deploy verb + a couple of sub-verbs.
			"deploy", "logs", "restart", "token", "fingerprint",
			// other verb groups the stale scripts lacked.
			"gateway", "image", "runner",
		}
		for _, w := range wants {
			if !strings.Contains(s, w) {
				t.Errorf("%s: generated completion missing %q", shell, w)
			}
		}
		// Hidden commands never leak as candidates.
		if strings.Contains(s, "__supervise") {
			t.Errorf("%s: hidden __supervise leaked into completion", shell)
		}
	}
}

// TestRunComplete verifies the hidden `sklp __complete <verb>` helper
// resolves stacks-for-verb and pipelines from a .sklp/ tree, sorted and
// deterministic, with no error on unknown subjects.
func TestRunComplete(t *testing.T) {
	repo := t.TempDir()
	write := func(rel string) {
		p := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("name: x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".sklp/core/dev.yaml")
	write(".sklp/transcript/deploy.yaml")
	write(".sklp/pipelines/ci.yaml")

	cases := []struct {
		name string
		args []string
		want string // exact stdout
	}{
		{"dev lists stacks with dev.yaml", []string{"dev"}, "core\n"},
		{"deploy lists stacks with deploy.yaml", []string{"deploy"}, "transcript\n"},
		{"run lists pipelines", []string{"run"}, "ci\n"},
		{"unknown subject is silent", []string{"frobnicate"}, ""},
		{"no args is silent", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if code := runCompleteTo(&out, repo, tc.args); code != 0 {
				t.Fatalf("exit = %d, want 0", code)
			}
			if out.String() != tc.want {
				t.Fatalf("stdout = %q, want %q", out.String(), tc.want)
			}
		})
	}
}
