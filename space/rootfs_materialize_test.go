package space

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestMaterializeRootfs_ConcurrentCallersBuildOnce(t *testing.T) {
	cacheRoot := t.TempDir()
	var fills atomic.Int32
	fill := func(staging string) error {
		fills.Add(1)
		for i := 0; i < 50; i++ {
			if err := os.WriteFile(filepath.Join(staging, fmt.Sprintf("f%02d", i)), []byte("x"), 0o644); err != nil {
				return err
			}
			time.Sleep(time.Millisecond)
		}
		return nil
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := materializeRootfs(cacheRoot, "abc", fill); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("materializeRootfs: %v", err)
	}
	if n := fills.Load(); n != 1 {
		t.Fatalf("fill ran %d times, want once", n)
	}
	dst := filepath.Join(cacheRoot, "rootfs", "abc")
	entries, err := os.ReadDir(dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 51 {
		t.Fatalf("rootfs has %d entries, want 50 files + sentinel", len(entries))
	}
	if _, err := os.Stat(dst + ".staging"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging dir left behind: %v", err)
	}
}

func TestMaterializeRootfs_FailedFillLeavesNoRootfs(t *testing.T) {
	cacheRoot := t.TempDir()
	boom := errors.New("registry went away")
	_, err := materializeRootfs(cacheRoot, "abc", func(staging string) error {
		_ = os.WriteFile(filepath.Join(staging, "half"), nil, 0o644)
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the fill error", err)
	}
	for _, p := range []string{"abc", "abc.staging"} {
		if _, err := os.Stat(filepath.Join(cacheRoot, "rootfs", p)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s left behind after a failed fill: %v", p, err)
		}
	}
	ran := false
	if _, err := materializeRootfs(cacheRoot, "abc", func(string) error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("second attempt: ran=%v err=%v, want a fresh fill", ran, err)
	}
}

type bytesFetcher struct{ body []byte }

func (b bytesFetcher) Fetch(context.Context, ocispec.Descriptor) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(b.body)), nil
}

func TestFetchLayerBlob_RejectsBytesNotMatchingTheDigest(t *testing.T) {
	dir := t.TempDir()
	good := []byte("layer bytes")
	desc := ocispec.Descriptor{Digest: digest.FromBytes(good), Size: int64(len(good))}

	path := filepath.Join(dir, desc.Digest.Encoded())
	if err := fetchLayerBlob(context.Background(), bytesFetcher{body: good[:5]}, desc, path); err == nil {
		t.Fatal("a truncated download was accepted")
	}
	for _, p := range []string{path, path + ".part"} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s left behind after a rejected download: %v", p, err)
		}
	}
	if err := fetchLayerBlob(context.Background(), bytesFetcher{body: good}, desc, path); err != nil {
		t.Fatalf("intact download rejected: %v", err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, good) {
		t.Fatalf("blob on disk = %q, want the layer bytes", got)
	}
}
