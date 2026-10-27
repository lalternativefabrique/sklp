package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCacheStatus_RemoteDisabled(t *testing.T) {
	t.Setenv("SKLP_REMOTE_CACHE_URL", "")
	t.Setenv("SKLP_REMOTE_CACHE_BUCKET", "")
	repo := t.TempDir()
	// Plant a fake local cache file so the size walker has something
	// to report.
	cacheDir := filepath.Join(repo, ".sklp", "cache", "runs", "abc")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "result.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := writeCacheStatus(context.Background(), &out, repo); err != nil {
		t.Fatalf("writeCacheStatus: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "local cache:") {
		t.Errorf("missing local section: %s", got)
	}
	if !strings.Contains(got, "remote cache: disabled") {
		t.Errorf("missing remote disabled line: %s", got)
	}
	if !strings.Contains(got, "(none)") {
		t.Errorf("missing last upload line: %s", got)
	}
}

func TestCacheStatus_RemoteEnabled(t *testing.T) {
	t.Setenv("SKLP_REMOTE_CACHE_URL", "https://s3.fr-par.scw.cloud")
	t.Setenv("SKLP_REMOTE_CACHE_BUCKET", "my-bucket")
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIA")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_REGION", "fr-par")

	repo := t.TempDir()
	var out bytes.Buffer
	if err := writeCacheStatus(context.Background(), &out, repo); err != nil {
		t.Fatalf("writeCacheStatus: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "https://s3.fr-par.scw.cloud") || !strings.Contains(got, "my-bucket") {
		t.Errorf("expected endpoint + bucket in output, got: %s", got)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{2048, "2.00 KiB"},
	}
	for _, c := range cases {
		if got := humanBytes(c.in); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseCacheSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		bad  bool
	}{
		{in: "", want: -1},
		{in: "0", want: 0},
		{in: "4096", want: 4096},
		{in: "12G", want: 12 << 30},
		{in: "512m", want: 512 << 20},
		{in: "2T", want: 2 << 40},
		{in: "-1", bad: true},
		{in: "big", bad: true},
	}
	for _, c := range cases {
		got, err := parseCacheSize(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("parseCacheSize(%q) = %d, want an error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseCacheSize(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseCacheSize(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}
