package remotecache

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNew_DisabledWhenEnvUnset(t *testing.T) {
	t.Setenv("SKLP_REMOTE_CACHE_URL", "")
	t.Setenv("SKLP_REMOTE_CACHE_BUCKET", "")
	rc, err := New(context.Background())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if rc != nil {
		t.Fatalf("expected nil RemoteCache when env unset, got %T", rc)
	}
}

func TestNew_RejectsMissingCreds(t *testing.T) {
	t.Setenv("SKLP_REMOTE_CACHE_URL", "https://example.invalid")
	t.Setenv("SKLP_REMOTE_CACHE_BUCKET", "b")
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	if _, err := New(context.Background()); err == nil {
		t.Fatal("expected error when creds missing")
	}
}

func TestFake_CASRoundTrip(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	body := []byte("hello world")
	if err := f.PutCAS(ctx, "sha", bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatalf("put: %v", err)
	}
	rc, err := f.GetCAS(ctx, "sha")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, body) {
		t.Fatalf("got %q want %q", got, body)
	}
	if ok, _ := f.HasCAS(ctx, "sha"); !ok {
		t.Fatal("HasCAS false after Put")
	}
	if ok, _ := f.HasCAS(ctx, "missing"); ok {
		t.Fatal("HasCAS true for missing key")
	}
}

func TestFake_CASIdempotent(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	body := []byte("x")
	_ = f.PutCAS(ctx, "k", bytes.NewReader(body), 1)
	_ = f.PutCAS(ctx, "k", bytes.NewReader(body), 1)
	if n := f.PutCASCount(); n != 1 {
		t.Fatalf("expected 1 PutCAS persistence, got %d", n)
	}
}

func TestFake_GetCASNotFound(t *testing.T) {
	_, err := NewFake().GetCAS(context.Background(), "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestFake_ResultRoundTrip(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	blob := &StepResultBlob{
		Name:       "build",
		ExitCode:   0,
		Status:     "passed",
		DurationMS: 12,
		Outputs:    []OutputEntry{{Path: "dist/x", Digest: "deadbeef"}},
		Stdout:     []byte("stdout"),
		Stderr:     []byte("stderr"),
	}
	if err := f.PutResult(ctx, "step-key", blob); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := f.GetResult(ctx, "step-key")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "build" || got.ExitCode != 0 || got.Status != "passed" || got.DurationMS != 12 {
		t.Fatalf("bad blob: %+v", got)
	}
	if len(got.Outputs) != 1 || got.Outputs[0].Path != "dist/x" || got.Outputs[0].Digest != "deadbeef" {
		t.Fatalf("bad outputs: %+v", got.Outputs)
	}
	if string(got.Stdout) != "stdout" || string(got.Stderr) != "stderr" {
		t.Fatalf("logs not preserved: %q %q", got.Stdout, got.Stderr)
	}
}

func TestFake_GetResultNotFound(t *testing.T) {
	_, err := NewFake().GetResult(context.Background(), "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestFake_FailureModes(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	f.FailPutCAS = true
	if err := f.PutCAS(ctx, "k", bytes.NewReader([]byte("x")), 1); err == nil {
		t.Fatal("expected PutCAS failure")
	}
	f.FailPutCAS = false
	f.FailPutResult = true
	if err := f.PutResult(ctx, "s", &StepResultBlob{}); err == nil {
		t.Fatal("expected PutResult failure")
	}
}

func TestLastUpload_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := TouchLastUpload(dir); err != nil {
		t.Fatalf("touch: %v", err)
	}
	got, err := ReadLastUpload(dir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if time.Since(got) > time.Minute {
		t.Fatalf("stamp suspiciously old: %v", got)
	}
}

func TestLastUpload_MissingFile(t *testing.T) {
	dir := t.TempDir()
	got, err := ReadLastUpload(dir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("expected zero time, got %v", got)
	}
}

func TestLastUploadPath(t *testing.T) {
	dir := t.TempDir()
	want := filepath.Join(dir, ".sklp", "cache", ".last-upload")
	if got := LastUploadPath(dir); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	// Sanity check file flow.
	_ = os.MkdirAll(filepath.Dir(want), 0o700)
	_ = os.WriteFile(want, []byte("2025-01-01T00:00:00Z"), 0o600)
}
