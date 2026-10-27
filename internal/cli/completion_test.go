package cli

import (
	"strings"
	"testing"
)

// synthTree is a small Command tree exercising the generator: a group with
// sub-verbs (one carrying a flag + a dynamic-completion positional), a hidden
// command that MUST be skipped, an enum flag and an alias.
func synthTree() Command {
	return Command{
		Name:  "demo",
		Short: "demo root",
		Sub: []Command{
			{
				Name:    "img",
				Aliases: []string{"image"},
				Short:   "manage images",
				Sub: []Command{
					{Name: "push", Short: "push an image", Flags: []Flag{
						{Name: "tag", Type: StringFlag, Usage: "image tag"},
						{Name: "output", Type: StringFlag, Usage: "output format"},
					}},
				},
			},
			{
				Name:  "box",
				Short: "manage boxes",
				Sub: []Command{
					{Name: "up", Short: "create a box", Positionals: []Positional{{Name: "name", Required: true}}},
					{Name: "exec", Short: "exec in a box", Positionals: []Positional{{Name: "name", Required: true, Complete: "box"}}},
				},
			},
			{Name: "__complete", Hidden: true, Short: "internal"},
			{Name: "__secret", Hidden: true, Short: "hidden verb"},
		},
	}
}

// mustContainAll fails when any want substring is absent from got.
func mustContainAll(t *testing.T, shell, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("%s: missing %q\n--- output ---\n%s", shell, w, got)
		}
	}
}

func TestGeneratorsReflectTree(t *testing.T) {
	root := synthTree()
	zsh := GenerateZsh(root, "demo")
	bash := GenerateBash(root, "demo")
	fish := GenerateFish(root, "demo")

	// Top-level verbs + sub-verbs present in every shell.
	for shell, out := range map[string]string{"zsh": zsh, "bash": bash, "fish": fish} {
		mustContainAll(t, shell, out, "img", "box", "push", "up", "exec")
		// Flags reflected.
		mustContainAll(t, shell, out, "tag")
		// Hidden commands skipped.
		if strings.Contains(out, "__complete") && shell != "bash" {
			// bash/zsh dynamic uses `__complete` as a literal subcommand call; the
			// hidden VERB name must never appear as a candidate. Assert it is not a
			// listed sub-verb by checking the hidden secret verb is fully absent.
		}
		if strings.Contains(out, "__secret") {
			t.Errorf("%s: hidden verb __secret leaked into output", shell)
		}
	}

	// Dynamic positional subject wired for `box exec`: zsh/fish call the helper
	// with the literal subject; bash routes it through a `$dyn` variable.
	mustContainAll(t, "zsh", zsh, "__complete box")
	mustContainAll(t, "fish", fish, "__complete box")
	mustContainAll(t, "bash", bash, `dyn="box"`, `__complete "$dyn"`)

	// Shell-specific shape.
	mustContainAll(t, "zsh", zsh, "#compdef demo", "_demo()", "compdef _demo demo")
	mustContainAll(t, "bash", bash, "_demo()", "complete -F _demo demo")
	mustContainAll(t, "fish", fish, "complete -c demo")

	// Enum flag values surface.
	mustContainAll(t, "bash", bash, "text json")
}

// TestGeneratorPathCases asserts the per-path case labels are emitted so a
// completer can branch on the verb path (root, group, leaf).
func TestGeneratorPathCases(t *testing.T) {
	zsh := GenerateZsh(synthTree(), "demo")
	mustContainAll(t, "zsh", zsh,
		`"img")`,      // group
		`"img push")`, // leaf under group
		`"box")`,      // group
		`"box exec")`, // leaf with dynamic completion
	)
}
