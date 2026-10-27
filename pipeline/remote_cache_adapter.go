package pipeline

import (
	"context"
	"errors"
	"io"

	"github.com/lalternativefabrique/sklp/internal/plan"
	"github.com/lalternativefabrique/sklp/internal/remotecache"
)

// remoteCacheAdapter bridges the transport-level interface
// (remotecache.RemoteCache) to the executor-level interface
// (plan.RemoteCache). The two wire shapes are intentionally identical;
// the adapter exists only to keep internal/plan from importing
// internal/remotecache (which would couple the executor to the AWS
// SDK and complicate dep-pruned builds).
type remoteCacheAdapter struct {
	inner remotecache.RemoteCache
	repo  string
}

func newRemoteCacheAdapter(inner remotecache.RemoteCache, repo string) *remoteCacheAdapter {
	return &remoteCacheAdapter{inner: inner, repo: repo}
}

func (a *remoteCacheAdapter) GetResult(ctx context.Context, stepKey string) (*plan.RemoteStepResult, error) {
	blob, err := a.inner.GetResult(ctx, stepKey)
	if err != nil {
		if errors.Is(err, remotecache.ErrNotFound) {
			return nil, plan.ErrRemoteCacheMiss
		}
		return nil, err
	}
	outs := make([]plan.RemoteCacheOutput, 0, len(blob.Outputs))
	for _, o := range blob.Outputs {
		outs = append(outs, plan.RemoteCacheOutput{Path: o.Path, Digest: o.Digest})
	}
	return &plan.RemoteStepResult{
		Name:       blob.Name,
		ExitCode:   blob.ExitCode,
		Status:     blob.Status,
		DurationMS: blob.DurationMS,
		Outputs:    outs,
		Stdout:     blob.Stdout,
		Stderr:     blob.Stderr,
	}, nil
}

func (a *remoteCacheAdapter) PutResult(ctx context.Context, stepKey string, res *plan.RemoteStepResult) error {
	outs := make([]remotecache.OutputEntry, 0, len(res.Outputs))
	for _, o := range res.Outputs {
		outs = append(outs, remotecache.OutputEntry{Path: o.Path, Digest: o.Digest})
	}
	err := a.inner.PutResult(ctx, stepKey, &remotecache.StepResultBlob{
		Name:       res.Name,
		ExitCode:   res.ExitCode,
		Status:     res.Status,
		DurationMS: res.DurationMS,
		Outputs:    outs,
		Stdout:     res.Stdout,
		Stderr:     res.Stderr,
	})
	if err == nil && a.repo != "" {
		_ = remotecache.TouchLastUpload(a.repo)
	}
	return err
}

func (a *remoteCacheAdapter) GetCAS(ctx context.Context, sha string) (io.ReadCloser, error) {
	rc, err := a.inner.GetCAS(ctx, sha)
	if err != nil {
		if errors.Is(err, remotecache.ErrNotFound) {
			return nil, plan.ErrRemoteCacheMiss
		}
		return nil, err
	}
	return rc, nil
}

func (a *remoteCacheAdapter) PutCAS(ctx context.Context, sha string, r io.Reader, size int64) error {
	return a.inner.PutCAS(ctx, sha, r, size)
}

func (a *remoteCacheAdapter) HasCAS(ctx context.Context, sha string) (bool, error) {
	return a.inner.HasCAS(ctx, sha)
}
