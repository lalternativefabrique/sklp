package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/lalternativefabrique/sklp/spacemanager"
)

func TestTokenSource_ReloadsFileOnMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := writeTokenFile(path, "first"); err != nil {
		t.Fatal(err)
	}
	src := newTokenSource(path, "first")
	if !src.matches("first") || src.matches("second") {
		t.Fatal("initial token not honoured")
	}
	if _, err := RotateTokenAt(path); err != nil {
		t.Fatal(err)
	}
	tok, _ := readToken(path)
	if src.matches("first") {
		t.Error("old token still accepted after an out-of-process rotation")
	}
	if !src.matches(tok) {
		t.Error("new token from disk not accepted")
	}
}

func TestRotateRoute_SwapsTokenAndKillsOld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := writeTokenFile(path, testToken); err != nil {
		t.Fatal(err)
	}
	src := newTokenSource(path, testToken)
	reg := NewRegistry()
	reg.Register("production", &fakeSupervisor{ready: true, live: map[string]spacemanager.Service{}}, func() {})
	srv, fp := newTLSServerWithHandler(t, tokenAuthSource(src, newMultiMuxWithTokens(reg, nil, src)))
	c, err := DialNet(srv.URL, testToken, WithCAFingerprint(fp))
	if err != nil {
		t.Fatal(err)
	}
	next, err := c.RotateToken(context.Background())
	if err != nil {
		t.Fatalf("RotateToken: %v", err)
	}
	if next == testToken || next == "" {
		t.Fatalf("rotate returned %q", next)
	}
	if onDisk, _ := readToken(path); onDisk != next {
		t.Errorf("disk has %q, want %q", onDisk, next)
	}
	if _, err := c.Health(context.Background()); err == nil {
		t.Error("old token still works after rotation")
	}
	c2, err := DialNet(srv.URL, next, WithCAFingerprint(fp))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c2.Health(context.Background()); err != nil {
		t.Errorf("new token refused: %v", err)
	}
	resp, _ := http.Get(srv.URL + RouteTokenRotate)
	if resp != nil && resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated rotate: status %d", resp.StatusCode)
	}
}

func newTLSServerWithHandler(t *testing.T, h http.Handler) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	sum := sha256.Sum256(srv.Certificate().Raw)
	return srv, hex.EncodeToString(sum[:])
}
