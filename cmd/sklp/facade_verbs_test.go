package main

import (
	"reflect"
	"testing"

	"github.com/lalternativefabrique/sklp/internal/cli"
)

// findFacadeSub returns the named child of a command, failing the test if absent.
func findFacadeSub(t *testing.T, parent cli.Command, name string) cli.Command {
	t.Helper()
	for _, c := range parent.Sub {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("%s: no sub-command %q", parent.Name, name)
	return cli.Command{}
}

// TestSkillsFlags locks the skills declaration: defaults + every flag parses.
func TestSkillsFlags(t *testing.T) {
	a, err := cli.ParseLeaf(skillsCommand(), []string{"-target", "claude,codex", "-force", "-repo", "/x"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.String("target") != "claude,codex" || a.String("repo") != "/x" || !a.Bool("force") {
		t.Fatalf("skills flags: %+v", a)
	}
	// Defaults when omitted.
	d, _ := cli.ParseLeaf(skillsCommand(), nil)
	if d.String("target") != "claude,cursor" || d.String("repo") != "." {
		t.Fatalf("skills defaults: target=%q repo=%q", d.String("target"), d.String("repo"))
	}
}

// TestInitFlags covers the init flag set incl. -agent / -no-run.
func TestInitFlags(t *testing.T) {
	a, err := cli.ParseLeaf(initCommand(), []string{"-agent", "claude", "-no-run", "-yes"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.String("agent") != "claude" || !a.Bool("no-run") || !a.Bool("yes") {
		t.Fatalf("init flags: %+v", a)
	}
}

// TestVersionShortAlias asserts -s aliases -short.
func TestVersionShortAlias(t *testing.T) {
	for _, tok := range []string{"-short", "-s"} {
		a, err := cli.ParseLeaf(versionCommand(), []string{tok})
		if err != nil {
			t.Fatalf("parse %s: %v", tok, err)
		}
		if !a.Bool("short") {
			t.Fatalf("%s did not set short", tok)
		}
	}
}

// TestCacheStatusFlag checks the cache status -repo flag.
func TestCacheStatusFlag(t *testing.T) {
	status := findFacadeSub(t, cacheCommand(), "status")
	a, err := cli.ParseLeaf(status, []string{"-repo", "/r"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.String("repo") != "/r" {
		t.Fatalf("cache repo=%q", a.String("repo"))
	}
}

// TestRunnerBuildFlags checks the runner build flags incl. -print + -tag default.
func TestRunnerBuildFlags(t *testing.T) {
	build := findFacadeSub(t, runnerCommand(), "build")
	a, err := cli.ParseLeaf(build, []string{"-print", "-config", "c.yaml"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !a.Bool("print") || a.String("config") != "c.yaml" || a.String("tag") != "sklp-runner:dev" {
		t.Fatalf("runner build flags: %+v tag=%q", a, a.String("tag"))
	}
}

// TestAuthLoginFlags checks login's --api-url declaration. There is no
// --profile any more: auth writes the single "api" block, nothing to select.
func TestAuthLoginFlags(t *testing.T) {
	login := findFacadeSub(t, authCommand(), "login")
	a, err := cli.ParseLeaf(login, []string{"--api-url", "http://x"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.String("api-url") != "http://x" {
		t.Fatalf("auth login flags: %+v", a)
	}
	for _, f := range login.Flags {
		if f.Name == "profile" || f.Name == "context" {
			t.Fatalf("auth login must not declare a %q selector", f.Name)
		}
	}
}

// TestImagePushFlags covers the image push flag set + defaults.
func TestImagePushFlags(t *testing.T) {
	push := findFacadeSub(t, imageCommand(), "push")
	a, err := cli.ParseLeaf(push, []string{"--image", "reg/x", "--scan"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.String("image") != "reg/x" || !a.Bool("scan") || a.String("dockerfile") != "Dockerfile" || a.String("context") != "." {
		t.Fatalf("image push flags: %+v", a)
	}
}

// TestGatewaySubFlags covers the up/down/status -file/-repo flags.
func TestGatewaySubFlags(t *testing.T) {
	up := findFacadeSub(t, gatewayCommand(), "up")
	a, err := cli.ParseLeaf(up, []string{"-repo", "/r"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.String("repo") != "/r" || a.String("file") == "" {
		t.Fatalf("gateway up flags: %+v", a)
	}
}

// TestIssueEnvFallback asserts -project falls back to SKLP_PROJECT through the
// façade declaration. `sklp issue` carries no profile/context selector: it
// reads the single "api" block, so no deploy state can retarget it.
func TestIssueEnvFallback(t *testing.T) {
	t.Setenv("SKLP_PROJECT", "proj-env")
	list := findFacadeSub(t, issueCommand(), "list")
	a, err := cli.ParseLeaf(list, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.String("project") != "proj-env" {
		t.Fatalf("env fallback: project=%q", a.String("project"))
	}
	for _, f := range list.Flags {
		if f.Name == "profile" || f.Name == "context" {
			t.Fatalf("issue list must not declare a %q selector", f.Name)
		}
	}
	// Explicit flag wins over env.
	b, _ := cli.ParseLeaf(list, []string{"-project", "explicit"})
	if b.String("project") != "explicit" {
		t.Fatalf("explicit project=%q", b.String("project"))
	}
}

// TestIssueAttachRepeatable asserts -attach collects repeated values.
func TestIssueAttachRepeatable(t *testing.T) {
	newCmd := findFacadeSub(t, issueCommand(), "new")
	a, err := cli.ParseLeaf(newCmd, []string{"-attach", "a", "-attach", "b", "title"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !reflect.DeepEqual(a.StringSlice("attach"), []string{"a", "b"}) {
		t.Fatalf("attach=%v", a.StringSlice("attach"))
	}
	if got := a.Positionals(); len(got) != 1 || got[0] != "title" {
		t.Fatalf("positionals=%v", got)
	}
}

// TestIssueStatusTwoPositionals locks the <id> <status> pair.
func TestIssueStatusTwoPositionals(t *testing.T) {
	st := findFacadeSub(t, issueCommand(), "status")
	a, err := cli.ParseLeaf(st, []string{"-project", "p", "i-1", "done"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := a.Positionals(); !reflect.DeepEqual(got, []string{"i-1", "done"}) {
		t.Fatalf("positionals=%v", got)
	}
}

// TestDeployLogsInterleavedPositional asserts the service positional parses
// whether it sits before or after -f, and that -f aliases -follow.
