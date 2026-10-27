package main

import (
	"reflect"
	"testing"
)

// sameCmd compares two command argvs treating nil and an empty slice as equal
// (parseExecArgs may return either for "no command").
func sameCmd(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

// TestParseExecArgs covers the docker/kubectl-style exec parsing: the service is
// the first positional, the command follows it WITH or WITHOUT `--`, known flags
// sit on either side of the service, and an empty command is left to the caller
// to default to sh. The parser is flag-free; the -space default is applied by
// the caller, so an unset -space surfaces here as "".
func TestParseExecArgs(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantSvc   string
		wantCmd   []string
		wantSpace string // "" = -space not given (caller defaults to production)
		wantErr   bool
	}{
		{"cmd without dashes", []string{"web", "sh"}, "web", []string{"sh"}, "", false},
		{"cmd after double dash", []string{"web", "--", "sh"}, "web", []string{"sh"}, "", false},
		{"multi-arg cmd after double dash", []string{"web", "--", "ls", "-la", "/"}, "web", []string{"ls", "-la", "/"}, "", false},
		{"multi-arg cmd without double dash", []string{"web", "ls", "-la"}, "web", []string{"ls", "-la"}, "", false},
		{"service only -> empty cmd (caller defaults sh)", []string{"web"}, "web", nil, "", false},
		{"flag after service then cmd", []string{"web", "-space", "prod", "sh"}, "web", []string{"sh"}, "prod", false},
		{"flag before service then cmd", []string{"-space", "prod", "web", "sh"}, "web", []string{"sh"}, "prod", false},
		{"flag before service + double dash cmd", []string{"-space", "prod", "web", "--", "ps", "aux"}, "web", []string{"ps", "aux"}, "prod", false},
		{"no args at all", []string{}, "", nil, "", false},
		{"only double dash gives literal cmd, no service", []string{"--", "sh"}, "", []string{"sh"}, "", false},
		{"unknown flag before service errors", []string{"-nope", "web", "sh"}, "", nil, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			flags, svc, cmd, err := parseExecArgs(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got svc=%q cmd=%v", svc, cmd)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if svc != tc.wantSvc {
				t.Errorf("service = %q, want %q", svc, tc.wantSvc)
			}
			if !sameCmd(cmd, tc.wantCmd) {
				t.Errorf("cmd = %v, want %v", cmd, tc.wantCmd)
			}
			if flags.space != tc.wantSpace {
				t.Errorf("space = %q, want %q", flags.space, tc.wantSpace)
			}
		})
	}
}

// TestParseExecArgsEmptyCmdDefaultsToSh asserts `exec web` yields an empty
// command that the caller turns into the interactive `sh` default.
func TestParseExecArgsEmptyCmdDefaultsToSh(t *testing.T) {
	_, svc, cmd, err := parseExecArgs([]string{"web"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if svc != "web" {
		t.Fatalf("service = %q, want web", svc)
	}
	if len(cmd) == 0 {
		cmd = []string{"sh"}
	}
	if !reflect.DeepEqual(cmd, []string{"sh"}) {
		t.Fatalf("defaulted cmd = %v, want [sh]", cmd)
	}
}
