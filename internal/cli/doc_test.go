package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// sampleTree mirrors the shapes the real rootCommand uses: a group with a
// hidden child, flags of every type, env fallbacks, and positionals.
func sampleTree() Command {
	return Command{
		Name:  "sklp",
		Short: "root",
		Sub: []Command{
			{
				Name:    "space",
				Short:   "manage spaces",
				Aliases: []string{"sp"},
				Sub: []Command{
					{
						Name:  "up",
						Short: "launch",
						Flags: []Flag{
							{Name: "repo", Type: StringFlag, StringDefault: ".", Usage: "repo root"},
							{Name: "force", Type: BoolFlag, BoolDefault: true, Usage: "force"},
							{Name: "n", Type: IntFlag, IntDefault: 3, Usage: "count"},
							{Name: "attach", Type: StringSliceFlag, Usage: "repeatable"},
							{Name: "profile", Type: StringFlag, EnvFallback: "SKLP_PROFILE", Usage: "p"},
						},
						Positionals: []Positional{{Name: "name", Required: true}},
					},
					{Name: "__hidden", Hidden: true, Short: "internal"},
					{Name: "down", Short: "stop", Positionals: []Positional{{Name: "name", Complete: "space"}}},
				},
			},
			{Name: "__complete", Hidden: true},
		},
	}
}

func TestBuildDocPrunesHidden(t *testing.T) {
	doc := BuildDoc(sampleTree())
	names := map[string]bool{}
	for _, s := range doc.Subcommands {
		names[s.Name] = true
	}
	if names["__complete"] {
		t.Fatal("hidden __complete leaked into doc tree")
	}
	if !names["space"] {
		t.Fatal("space command missing")
	}
	var space DocCommand
	for _, s := range doc.Subcommands {
		if s.Name == "space" {
			space = s
		}
	}
	for _, s := range space.Subcommands {
		if s.Name == "__hidden" {
			t.Fatal("hidden subcommand leaked into doc tree")
		}
	}
}

func TestBuildDocFlagTypesAndDefaults(t *testing.T) {
	doc := BuildDoc(sampleTree())
	up := findDocSub(t, findDocSub(t, doc, "space"), "up")

	byName := map[string]DocFlag{}
	for _, f := range up.Flags {
		byName[f.Name] = f
	}
	cases := []struct{ name, typ, def, env string }{
		{"repo", "string", ".", ""},
		{"force", "bool", "true", ""},
		{"n", "int", "3", ""},
		{"attach", "stringSlice", "", ""},
		{"profile", "string", "", "SKLP_PROFILE"},
	}
	for _, c := range cases {
		f, ok := byName[c.name]
		if !ok {
			t.Errorf("flag %q missing", c.name)
			continue
		}
		if f.Type != c.typ {
			t.Errorf("%s: type=%q want %q", c.name, f.Type, c.typ)
		}
		if f.Default != c.def {
			t.Errorf("%s: default=%q want %q", c.name, f.Default, c.def)
		}
		if f.EnvFallback != c.env {
			t.Errorf("%s: env=%q want %q", c.name, f.EnvFallback, c.env)
		}
	}
}

func TestBuildDocSlugAndSynopsis(t *testing.T) {
	doc := BuildDoc(sampleTree())
	up := findDocSub(t, findDocSub(t, doc, "space"), "up")
	if up.Slug != "space-up" {
		t.Errorf("slug=%q want space-up", up.Slug)
	}
	if up.Synopsis != "sklp space up [flags] <name>" {
		t.Errorf("synopsis=%q", up.Synopsis)
	}
	if doc.Slug != "" {
		t.Errorf("root slug should be empty, got %q", doc.Slug)
	}
}

func TestBuildDocSubcommandsSorted(t *testing.T) {
	doc := BuildDoc(sampleTree())
	space := findDocSub(t, doc, "space")
	if len(space.Subcommands) != 2 { // down, up (hidden pruned)
		t.Fatalf("got %d subs, want 2", len(space.Subcommands))
	}
	if space.Subcommands[0].Name != "down" || space.Subcommands[1].Name != "up" {
		t.Errorf("subcommands not sorted: %v", docSubNames(space))
	}
}

func TestWriteDocValidJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteDoc(&buf, sampleTree()); err != nil {
		t.Fatal(err)
	}
	var decoded DocCommand
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	// HTML escaping disabled: synopsis `<name>` must appear verbatim, NOT as the
	// stdlib's default < / > / & escapes.
	out := buf.String()
	for _, esc := range []string{"\\u003c", "\\u003e", "\\u0026"} {
		if strings.Contains(out, esc) {
			t.Errorf("WriteDoc HTML-escaped output (found %s); want raw", esc)
		}
	}
	if !strings.Contains(out, "<name>") {
		t.Error("synopsis angle brackets did not survive verbatim")
	}
}

func findDocSub(t *testing.T, parent DocCommand, name string) DocCommand {
	t.Helper()
	for _, s := range parent.Subcommands {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("subcommand %q not found under %q", name, parent.Name)
	return DocCommand{}
}

func docSubNames(c DocCommand) []string {
	var out []string
	for _, s := range c.Subcommands {
		out = append(out, s.Name)
	}
	return out
}
