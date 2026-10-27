package main

import (
	"flag"
	"testing"
)

// TestParseWithPositional verifies the lone positional (a service name) is
// accepted on either side of the flags, so `sklp deploy logs db -f` and
// `... -f db` behave alike — and that extra positionals are rejected.
func TestParseWithPositional(t *testing.T) {
	newFS := func() (*flag.FlagSet, *bool, *string) {
		fs := flag.NewFlagSet("logs", flag.ContinueOnError)
		follow := fs.Bool("f", false, "")
		space := fs.String("space", "production", "")
		return fs, follow, space
	}

	tests := []struct {
		name       string
		args       []string
		wantPos    string
		wantFollow bool
		wantCap    string
		wantErr    bool
	}{
		{"name before flags", []string{"db", "-f"}, "db", true, "production", false},
		{"name after flags", []string{"-f", "db"}, "db", true, "production", false},
		{"name between flags", []string{"-f", "db", "-space", "x"}, "db", true, "x", false},
		{"name only", []string{"db"}, "db", false, "production", false},
		{"no positional", []string{"-f"}, "", true, "production", false},
		{"extra positional", []string{"db", "extra"}, "", false, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fs, follow, space := newFS()
			pos, err := parseWithPositional(fs, tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got pos=%q", pos)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if pos != tc.wantPos {
				t.Errorf("positional = %q, want %q", pos, tc.wantPos)
			}
			if *follow != tc.wantFollow {
				t.Errorf("follow = %v, want %v", *follow, tc.wantFollow)
			}
			if *space != tc.wantCap {
				t.Errorf("space = %q, want %q", *space, tc.wantCap)
			}
		})
	}
}
