package main

import (
	"flag"
	"testing"

	"github.com/lalternativefabrique/sklp/internal/config"
)

// TestMaybeRunRemoteNetDetection: the interceptor takes over a control sub-verb
// only when --server/--addr is present; plain deploy, non-control verbs, and
// socket-mode sub-verbs are left to the normal flow.
func TestMaybeRunRemoteNetDetection(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"plain deploy", []string{"--space", "p"}, false},
		{"ls local socket", []string{"ls", "--space", "p"}, false},
		{"renamed ps no longer piloted", []string{"ps", "--server", "https://h:9443"}, false},
		{"token verb with server (not piloted)", []string{"token", "--space", "p"}, false},
		{"ls with server flag", []string{"ls", "--server", "https://h:9443"}, true},
		{"logs with addr alias", []string{"logs", "web", "--addr", "https://h:9443"}, true},
		{"delete with server=inline", []string{"delete", "--server=https://h:9443"}, true},
		{"down (removed) not piloted", []string{"down", "--server=https://h:9443"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// We only assert the take-over DECISION, not the run (which would dial).
			// detection mirrors maybeRunRemoteNet's guards.
			got := isControlSubverb(tc.args) && len(tc.args) > 1 && hasServerFlag(tc.args[1:])
			if got != tc.want {
				t.Fatalf("detection(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// isControlSubverb mirrors maybeRunRemoteNet's verb gate (test helper).
func isControlSubverb(args []string) bool {
	if len(args) == 0 || len(args[0]) == 0 || args[0][0] == '-' {
		return false
	}
	switch args[0] {
	case "ls", "logs", "exec", "restart", "delete":
		return true
	}
	return false
}

// TestNetFlagsResolveToken: precedence token-file > token > SKLP_CONTEXT_TOKEN,
// and an error when none is set (no anonymous network access).
//
// The env var is SKLP_CONTEXT_TOKEN, not SKLP_TOKEN: this bearer drives a
// node's deploy control API and must not be satisfiable by a SaaS PAT exported
// for `sklp issue`.
func TestNetFlagsResolveToken(t *testing.T) {
	t.Run("explicit token", func(t *testing.T) {
		nf := &netFlags{token: "abc"}
		got, err := nf.resolveToken()
		if err != nil || got != "abc" {
			t.Fatalf("resolveToken = (%q,%v)", got, err)
		}
	})
	t.Run("env fallback", func(t *testing.T) {
		t.Setenv(config.EnvContextToken, "env-tok")
		nf := &netFlags{}
		got, err := nf.resolveToken()
		if err != nil || got != "env-tok" {
			t.Fatalf("resolveToken = (%q,%v)", got, err)
		}
	})
	t.Run("saas token does not satisfy the deploy bearer", func(t *testing.T) {
		t.Setenv(config.EnvContextToken, "")
		t.Setenv(config.EnvToken, "saas-pat")
		nf := &netFlags{}
		if got, err := nf.resolveToken(); err == nil {
			t.Fatalf("SKLP_TOKEN satisfied a deploy bearer: got %q", got)
		}
	})
	t.Run("none set", func(t *testing.T) {
		t.Setenv(config.EnvContextToken, "")
		t.Setenv(config.EnvToken, "")
		nf := &netFlags{}
		if _, err := nf.resolveToken(); err == nil {
			t.Fatal("want error when no token set")
		}
	})
}

// TestNetFlagsDialClientRequiresTrustMode: a remote dial without a pin or
// --insecure is refused (a self-signed server cert would never verify silently).
func TestNetFlagsDialClientRequiresTrustMode(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("SKLP_PROFILE", "")
	t.Setenv("SKLP_TOKEN", "tok")
	nf := &netFlags{server: "https://h:9443"}
	if _, err := nf.dialClient("production"); err == nil {
		t.Fatal("want error requiring --ca-fingerprint or --insecure")
	}
}

// TestBindNetFlagsParse: the flags register and parse (server/addr alias, token,
// fingerprint, insecure).
func TestBindNetFlagsParse(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	nf := bindNetFlags(fs)
	if err := fs.Parse([]string{"--addr", "https://h:9443", "--ca-fingerprint", "ab", "--insecure"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !nf.remote() || nf.caFinger != "ab" || !nf.insecure {
		t.Fatalf("parsed netFlags = %+v", nf)
	}
}
