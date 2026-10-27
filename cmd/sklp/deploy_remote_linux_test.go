package main

import (
	"reflect"
	"testing"
)

// withTTY swaps the isTerminal probe for the duration of a test so the -t/-T
// decision is deterministic without a real terminal.
func withTTY(t *testing.T, tty bool) {
	t.Helper()
	orig := isTerminal
	isTerminal = func() bool { return tty }
	t.Cleanup(func() { isTerminal = orig })
}

// TestParseRemoteArgs covers transport-flag detection + stripping: --host /
// --remote (ssh:// and bare), --ssh-port, --identity, --remote-bin, inline =,
// and absence (handled=false). Deploy-related args must survive untouched.
func TestParseRemoteArgs(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		wantOK     bool
		wantTarget sshTarget
		wantDeploy []string
		wantErr    bool
	}{
		{
			name:       "no transport flag -> not handled",
			args:       []string{"-space", "production"},
			wantOK:     false,
			wantDeploy: []string{"-space", "production"},
			wantTarget: sshTarget{remoteBin: "sklp"},
		},
		{
			name:       "host with space kept",
			args:       []string{"--host", "me@vm", "-space", "production"},
			wantOK:     true,
			wantTarget: sshTarget{userHost: "me@vm", remoteBin: "sklp"},
			wantDeploy: []string{"-space", "production"},
		},
		{
			name:       "single-dash host alias",
			args:       []string{"-host", "me@vm"},
			wantOK:     true,
			wantTarget: sshTarget{userHost: "me@vm", remoteBin: "sklp"},
			wantDeploy: []string{},
		},
		{
			name:       "inline host=value",
			args:       []string{"--host=me@vm", "-space", "prod"},
			wantOK:     true,
			wantTarget: sshTarget{userHost: "me@vm", remoteBin: "sklp"},
			wantDeploy: []string{"-space", "prod"},
		},
		{
			name:       "remote ssh url with port",
			args:       []string{"--remote", "ssh://me@vm:2222", "-space", "prod"},
			wantOK:     true,
			wantTarget: sshTarget{userHost: "me@vm", port: 2222, remoteBin: "sklp"},
			wantDeploy: []string{"-space", "prod"},
		},
		{
			name:       "remote bare user@host (no scheme)",
			args:       []string{"--remote", "me@vm"},
			wantOK:     true,
			wantTarget: sshTarget{userHost: "me@vm", remoteBin: "sklp"},
			wantDeploy: []string{},
		},
		{
			name:       "all transport flags stripped, deploy kept",
			args:       []string{"ls", "--host", "me@vm", "--ssh-port", "2200", "--identity", "/k/id", "--remote-bin", "/opt/sklp", "-space", "prod"},
			wantOK:     true,
			wantTarget: sshTarget{userHost: "me@vm", port: 2200, identity: "/k/id", remoteBin: "/opt/sklp"},
			wantDeploy: []string{"ls", "-space", "prod"},
		},
		{
			name:    "host without value",
			args:    []string{"--host"},
			wantErr: true,
		},
		{
			name:    "bad ssh-port",
			args:    []string{"--host", "me@vm", "--ssh-port", "abc"},
			wantErr: true,
		},
		{
			name:    "bad remote port",
			args:    []string{"--remote", "ssh://me@vm:nope"},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target, deploy, ok, err := parseRemoteArgs(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got none")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if target != tc.wantTarget {
				t.Fatalf("target = %+v, want %+v", target, tc.wantTarget)
			}
			if !reflect.DeepEqual(deploy, tc.wantDeploy) {
				t.Fatalf("deployArgs = %v, want %v", deploy, tc.wantDeploy)
			}
		})
	}
}

// TestBuildSSHCommand verifies the exact ssh argv: -t only for live sub-verbs
// AND a TTY; -T otherwise; -p/-i ordering; transport flags stripped while the
// sub-verb / --space / -f are forwarded to the remote sklp deploy.
func TestBuildSSHCommand(t *testing.T) {
	cases := []struct {
		name       string
		tty        bool
		target     sshTarget
		deployArgs []string
		wantArgv   []string // includes the "ssh" arg0
	}{
		{
			name:       "deploy on tty -> -t",
			tty:        true,
			target:     sshTarget{userHost: "me@vm", remoteBin: "sklp"},
			deployArgs: []string{"-space", "production"},
			wantArgv:   []string{"ssh", "-t", "me@vm", "--", "sklp", "deploy", "-space", "production"},
		},
		{
			name:       "deploy without tty -> -T",
			tty:        false,
			target:     sshTarget{userHost: "me@vm", remoteBin: "sklp"},
			deployArgs: []string{"-space", "production"},
			wantArgv:   []string{"ssh", "-T", "me@vm", "--", "sklp", "deploy", "-space", "production"},
		},
		{
			name:       "ls -> no pty even on tty",
			tty:        true,
			target:     sshTarget{userHost: "me@vm", remoteBin: "sklp"},
			deployArgs: []string{"ls", "-space", "production"},
			wantArgv:   []string{"ssh", "-T", "me@vm", "--", "sklp", "deploy", "ls", "-space", "production"},
		},
		{
			name:       "logs -f on tty -> -t",
			tty:        true,
			target:     sshTarget{userHost: "me@vm", remoteBin: "sklp"},
			deployArgs: []string{"logs", "web", "-f", "-space", "production"},
			wantArgv:   []string{"ssh", "-t", "me@vm", "--", "sklp", "deploy", "logs", "web", "-f", "-space", "production"},
		},
		{
			name:       "logs without -f -> no pty",
			tty:        true,
			target:     sshTarget{userHost: "me@vm", remoteBin: "sklp"},
			deployArgs: []string{"logs", "web", "-space", "production"},
			wantArgv:   []string{"ssh", "-T", "me@vm", "--", "sklp", "deploy", "logs", "web", "-space", "production"},
		},
		{
			name:       "delete on tty -> -t",
			tty:        true,
			target:     sshTarget{userHost: "me@vm", remoteBin: "sklp"},
			deployArgs: []string{"delete", "-space", "production"},
			wantArgv:   []string{"ssh", "-t", "me@vm", "--", "sklp", "deploy", "delete", "-space", "production"},
		},
		{
			name:       "port + identity + remote-bin",
			tty:        true,
			target:     sshTarget{userHost: "me@vm", port: 2222, identity: "/k/id", remoteBin: "/opt/sklp"},
			deployArgs: []string{"-space", "production"},
			wantArgv:   []string{"ssh", "-t", "-p", "2222", "-i", "/k/id", "me@vm", "--", "/opt/sklp", "deploy", "-space", "production"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withTTY(t, tc.tty)
			cmd := buildSSHCommand(tc.target, tc.deployArgs)
			if !reflect.DeepEqual(cmd.Args, tc.wantArgv) {
				t.Fatalf("ssh argv =\n  %v\nwant\n  %v", cmd.Args, tc.wantArgv)
			}
		})
	}
}

// TestWantsTTY isolates the live-vs-capturable decision per sub-verb/flag,
// independent of whether the local side is a terminal.
func TestWantsTTY(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"plain deploy", []string{"-space", "prod"}, true},
		{"delete", []string{"delete", "-space", "prod"}, true},
		{"ls", []string{"ls", "-space", "prod"}, false},
		{"logs follow", []string{"logs", "web", "-f"}, true},
		{"logs follow long", []string{"logs", "web", "--follow"}, true},
		{"logs no follow", []string{"logs", "web"}, false},
		{"exec", []string{"exec", "web"}, true},
		{"exec with cmd", []string{"exec", "web", "--", "ls"}, true},
		{"restart", []string{"restart", "web"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := wantsTTY(tc.args); got != tc.want {
				t.Fatalf("wantsTTY(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// TestRemoteBinDefault: SKLP_REMOTE_BIN wins over the "sklp" default.
func TestRemoteBinDefault(t *testing.T) {
	t.Setenv("SKLP_REMOTE_BIN", "")
	if got := remoteBinDefault(); got != "sklp" {
		t.Fatalf("default = %q, want sklp", got)
	}
	t.Setenv("SKLP_REMOTE_BIN", "/usr/local/bin/sklp")
	if got := remoteBinDefault(); got != "/usr/local/bin/sklp" {
		t.Fatalf("env override = %q, want /usr/local/bin/sklp", got)
	}
}
