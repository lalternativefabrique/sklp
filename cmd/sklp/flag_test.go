package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lalternativefabrique/sklp/internal/cli"
)

func flagArgs(t *testing.T, sub string, argv ...string) cli.Args {
	t.Helper()
	for _, c := range flagCommand().Sub {
		if c.Name == sub {
			a, err := cli.ParseLeaf(c, argv)
			if err != nil {
				t.Fatalf("parse flag %s %v: %v", sub, argv, err)
			}
			return a
		}
	}
	t.Fatalf("unknown flag sub-verb %q", sub)
	return nil
}

func flagServer(t *testing.T, patched *map[string]any, patchedPath *string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/projects/proj1/toggles":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": "t-2", "key": "zeta", "enabled": true},
				{"id": "t-1", "key": "alpha", "enabled": false, "description": "first"},
			})
		case r.Method == http.MethodPatch:
			*patchedPath = r.URL.Path
			_ = json.NewDecoder(r.Body).Decode(patched)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "t-1"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	writeAPIConfig(t, srv.URL)
}

func TestFlagList_SortsByKey(t *testing.T) {
	var body map[string]any
	var path string
	flagServer(t, &body, &path)

	out := &bytes.Buffer{}
	iio := &issueIO{Stdout: out, Stderr: io.Discard}
	if err := flagList(iio, flagArgs(t, "list", "--project", "proj1", "--plain")); err != nil {
		t.Fatalf("list: %v", err)
	}
	if want := "alpha\tfalse\tfirst\nzeta\ttrue\t\n"; out.String() != want {
		t.Fatalf("got %q, want %q", out.String(), want)
	}
}

func TestFlagSet_ResolvesKeyAndSendsState(t *testing.T) {
	for _, tc := range []struct {
		sub  string
		want bool
	}{{"enable", true}, {"disable", false}} {
		t.Run(tc.sub, func(t *testing.T) {
			var body map[string]any
			var path string
			flagServer(t, &body, &path)

			iio := &issueIO{Stdout: io.Discard, Stderr: io.Discard}
			if err := flagSet(iio, flagArgs(t, tc.sub, "--project", "proj1", "alpha"), tc.want); err != nil {
				t.Fatalf("%s: %v", tc.sub, err)
			}
			if path != "/api/projects/proj1/toggles/t-1" {
				t.Fatalf("unexpected path %q", path)
			}
			if body["enabled"] != tc.want {
				t.Fatalf("enabled = %v, want %v", body["enabled"], tc.want)
			}
		})
	}
}

func TestFlagSet_UnknownKeyDoesNotPatch(t *testing.T) {
	var body map[string]any
	var path string
	flagServer(t, &body, &path)

	iio := &issueIO{Stdout: io.Discard, Stderr: io.Discard}
	err := flagSet(iio, flagArgs(t, "enable", "--project", "proj1", "nope"), true)
	if err == nil || !strings.Contains(err.Error(), "flag nope not found") {
		t.Fatalf("got %v", err)
	}
	if path != "" {
		t.Fatalf("patched %q on an unknown key", path)
	}
}

func TestFlagCommand_RegisteredUnderRoot(t *testing.T) {
	for _, c := range rootCommand().Sub {
		if c.Name == "flag" {
			return
		}
	}
	t.Fatal("flag command not registered under rootCommand()")
}
