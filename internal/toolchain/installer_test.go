package toolchain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubTool lets the installer tests exercise Install() without depending
// on a real Tool implementation (kubectl/helm/go live in a sibling
// package and importing them would create a circular dep).
type stubTool struct {
	name      string
	urlFn     func(version, os, arch string) (string, error)
	shaFn     func(version, os, arch string) (string, error)
	extractFn func(archivePath, destDir string) (string, error)
}

func (s *stubTool) Name() string { return s.name }
func (s *stubTool) URL(v, o, a string) (string, error) {
	return s.urlFn(v, o, a)
}
func (s *stubTool) SHA256(v, o, a string) (string, error) {
	return s.shaFn(v, o, a)
}
func (s *stubTool) Extract(archive, dest string) (string, error) {
	return s.extractFn(archive, dest)
}

// registerForTest registers t and returns a cleanup that removes it.
// Using direct map access (with the lock) avoids the Register panic
// when a previous test left a registration behind.
func registerForTest(t *testing.T, tool Tool) {
	t.Helper()
	regMu.Lock()
	registry[tool.Name()] = tool
	regMu.Unlock()
	t.Cleanup(func() {
		regMu.Lock()
		delete(registry, tool.Name())
		regMu.Unlock()
	})
}

func TestInstall_HappyPath(t *testing.T) {
	// The fake "binary" served by the test server. Sha256 computed on
	// it is what the stubTool's SHA256() returns, so verification
	// passes and Install succeeds.
	payload := []byte("#!/bin/sh\necho stub-binary\n")
	digest := sha256.Sum256(payload)
	expectedHex := hex.EncodeToString(digest[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)

	registerForTest(t, &stubTool{
		name:  "stubtool",
		urlFn: func(_, _, _ string) (string, error) { return srv.URL, nil },
		shaFn: func(_, _, _ string) (string, error) { return expectedHex, nil },
		extractFn: func(archive, dest string) (string, error) {
			out := filepath.Join(dest, "stubtool")
			return out, CopyBinary(archive, out)
		},
	})

	bin, err := Install(context.Background(), "stubtool", "1.0.0", t.TempDir())
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("bin not on disk at %s: %v", bin, err)
	}
}

func TestInstall_SHAMismatchFails(t *testing.T) {
	payload := []byte("real content")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)

	wrongHex := strings.Repeat("a", 64)

	registerForTest(t, &stubTool{
		name:  "badtool",
		urlFn: func(_, _, _ string) (string, error) { return srv.URL, nil },
		shaFn: func(_, _, _ string) (string, error) { return wrongHex, nil },
		extractFn: func(archive, dest string) (string, error) {
			return "", nil // never reached
		},
	})

	_, err := Install(context.Background(), "badtool", "1.0.0", t.TempDir())
	if err == nil {
		t.Fatal("expected sha256 mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "sha256") {
		t.Errorf("error doesn't mention sha256: %v", err)
	}
}

func TestInstall_CacheHitSkipsNetwork(t *testing.T) {
	payload := []byte("cached")
	digest := sha256.Sum256(payload)
	expectedHex := hex.EncodeToString(digest[:])

	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)

	registerForTest(t, &stubTool{
		name:  "cachetool",
		urlFn: func(_, _, _ string) (string, error) { return srv.URL, nil },
		shaFn: func(_, _, _ string) (string, error) { return expectedHex, nil },
		extractFn: func(archive, dest string) (string, error) {
			out := filepath.Join(dest, "cachetool")
			return out, CopyBinary(archive, out)
		},
	})

	cache := t.TempDir()
	for i := 0; i < 3; i++ {
		if _, err := Install(context.Background(), "cachetool", "1.0.0", cache); err != nil {
			t.Fatalf("install %d: %v", i, err)
		}
	}
	if hits != 1 {
		t.Errorf("expected 1 network hit, got %d", hits)
	}
}

func TestInstall_UnknownToolErrors(t *testing.T) {
	_, err := Install(context.Background(), "no-such-tool", "1.0.0", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "not in native registry") {
		t.Fatalf("expected not-in-registry error, got %v", err)
	}
}

func TestInstall_BadHTTPStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	registerForTest(t, &stubTool{
		name:  "errtool",
		urlFn: func(_, _, _ string) (string, error) { return srv.URL, nil },
		shaFn: func(_, _, _ string) (string, error) { return strings.Repeat("a", 64), nil },
		extractFn: func(archive, dest string) (string, error) {
			return "", nil
		},
	})

	_, err := Install(context.Background(), "errtool", "1.0.0", t.TempDir())
	if err == nil {
		t.Fatal("expected http error, got nil")
	}
}

func TestRegisterGetNames(t *testing.T) {
	tool := &stubTool{name: "registertest"}
	registerForTest(t, tool)

	got, ok := Get("registertest")
	if !ok || got.Name() != "registertest" {
		t.Errorf("Get returned %v, %v", got, ok)
	}

	found := false
	for _, n := range Names() {
		if n == "registertest" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Names() missing registered tool")
	}
}
