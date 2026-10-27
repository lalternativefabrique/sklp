package imagebuild

import "testing"

func TestExpand(t *testing.T) {
	vars := Vars{"VERSION": "v4.19.1", "EMPTY": "", "NAME": "migrate"}
	cases := []struct {
		in, want string
	}{
		{"--branch ${VERSION}", "--branch v4.19.1"},
		{"--branch $VERSION x", "--branch v4.19.1 x"},
		{"${NAME}:${VERSION}", "migrate:v4.19.1"},
		{"${MISSING}", ""},                   // unknown -> empty (Docker)
		{"a${MISSING}b", "ab"},               // unknown mid-string
		{"${VERSION:-fallback}", "v4.19.1"},  // set -> value
		{"${MISSING:-fallback}", "fallback"}, // unset -> default
		{"${EMPTY:-fallback}", "fallback"},   // empty -> default
		{"${VERSION:+yes}", "yes"},           // set -> alt
		{"${MISSING:+yes}", ""},              // unset -> empty
		{"${EMPTY:+yes}", ""},                // empty -> empty
		{"price is $$5", "price is $5"},      // $$ escape
		{"$ alone", "$ alone"},               // bare $ not a name
		{"trailing$", "trailing$"},           // $ at EOL
		{"${unterminated", "${unterminated"}, // unterminated brace
		{"no vars here", "no vars here"},     // passthrough
	}
	for _, c := range cases {
		if got := Expand(c.in, vars); got != c.want {
			t.Errorf("Expand(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseArg(t *testing.T) {
	cases := []struct {
		in, name, def string
		hasDefault    bool
	}{
		{"MIGRATE_VERSION=v4.19.1", "MIGRATE_VERSION", "v4.19.1", true},
		{"FOO", "FOO", "", false},
		{`Q="quoted val"`, "Q", "quoted val", true},
		{"  SPACED = x ", "SPACED", "x", true},
	}
	for _, c := range cases {
		n, d, has := ParseArg(c.in)
		if n != c.name || d != c.def || has != c.hasDefault {
			t.Errorf("ParseArg(%q) = (%q,%q,%v), want (%q,%q,%v)", c.in, n, d, has, c.name, c.def, c.hasDefault)
		}
	}
}

func TestParse_GlobalArgBeforeFrom(t *testing.T) {
	df, err := Parse("ARG GO_VERSION=1.25\nFROM golang:${GO_VERSION}\nRUN true\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(df.GlobalArgs) != 1 {
		t.Fatalf("GlobalArgs = %d, want 1", len(df.GlobalArgs))
	}
	if got := df.GlobalArgs[0].Args; got != "GO_VERSION=1.25" {
		t.Errorf("global arg = %q", got)
	}
}

func TestParse_NonArgBeforeFromStillErrors(t *testing.T) {
	if _, err := Parse("RUN true\nFROM x\n"); err == nil {
		t.Fatal("expected error for RUN before FROM")
	}
}
