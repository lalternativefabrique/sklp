package plan

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// tryRemoteHit attempts to satisfy a step from the remote cache after a
// local miss. On a true hit (result downloaded, every CAS object
// pulled, working tree restored, local cache populated) it returns
// (outcome, true). Any transport / consistency failure returns
// (_, false) so the caller falls through to a normal execution.
func tryRemoteHit(
	ctx context.Context,
	cfg CIExecConfig,
	step CIStep,
	stepKey string,
	stdoutW, stderrW io.Writer,
) (stepOutcome, bool) {
	res := CIStepResult{Name: step.Name}
	blob, err := cfg.Remote.GetResult(ctx, stepKey)
	if err != nil {
		if !errors.Is(err, ErrRemoteCacheMiss) && stderrW != nil {
			fmt.Fprintf(stderrW, "[sklp] remote cache: get %s: %v (running normally)\n", step.Name, err)
		}
		notifyRemote(cfg, step.Name, "miss")
		return stepOutcome{}, false
	}
	notifyRemote(cfg, step.Name, "hit")

	// Pull every referenced CAS object into the local store so the
	// restore step (and subsequent local-hit runs) see them.
	for _, o := range blob.Outputs {
		if err := pullCASIfMissing(ctx, cfg, o.Digest); err != nil {
			if stderrW != nil {
				fmt.Fprintf(stderrW, "[sklp] remote cache: cas %s for step %q: %v (running normally)\n", short(o.Digest), step.Name, err)
			}
			return stepOutcome{}, false
		}
	}

	// Restore outputs into the working tree.
	localOutputs := make([]CIStepCacheOutput, 0, len(blob.Outputs))
	for _, o := range blob.Outputs {
		localOutputs = append(localOutputs, CIStepCacheOutput{Path: o.Path, Digest: o.Digest})
	}
	if cfg.CAS != nil {
		if err := restoreOutputs(cfg.CAS, cfg.Repo, localOutputs); err != nil {
			if stderrW != nil {
				fmt.Fprintf(stderrW, "[sklp] remote cache: restore %q: %v (running normally)\n", step.Name, err)
			}
			return stepOutcome{}, false
		}
	}

	// Replay logs into the caller's writers (same UX as a local hit).
	if rerr := Replay(blob.Stdout, blob.Stderr, stdoutW, stderrW); rerr != nil {
		return stepOutcome{}, false
	}

	// Populate the local step cache so the next run is a local hit.
	localRes := CIStepCacheResult{
		Name:       blob.Name,
		ExitCode:   blob.ExitCode,
		Status:     blob.Status,
		DurationMS: blob.DurationMS,
		Outputs:    localOutputs,
	}
	if cfg.Cache != nil {
		_ = cfg.Cache.StoreStep(cfg.PlanKey, stepKey, localRes, blob.Stdout, blob.Stderr)
	}

	dur := time.Duration(blob.DurationMS) * time.Millisecond
	res.Status = "cached"
	res.ExitCode = blob.ExitCode
	res.DurationMS = blob.DurationMS
	ev := CIStepEvent{Name: step.Name, Status: StatusCached, Duration: dur, Note: CachedNote + " (remote)"}
	return stepOutcome{Result: res, Event: ev, Cached: true}, true
}

// pullCASIfMissing downloads one CAS object from the remote into the
// local CAS only when the local copy is absent. Streams the body
// straight through cas.PutFile via a temp file to keep memory flat.
func pullCASIfMissing(ctx context.Context, cfg CIExecConfig, digest string) error {
	if cfg.CAS == nil {
		return errors.New("remote cache: local CAS is nil")
	}
	// Local hit: nothing to do. Read shortcuts via the CAS path layout
	// instead of opening a file we don't need.
	if _, err := os.Stat(cfg.CAS.pathFor(digest)); err == nil {
		return nil
	}
	rc, err := cfg.Remote.GetCAS(ctx, digest)
	if err != nil {
		return fmt.Errorf("get cas: %w", err)
	}
	defer rc.Close()
	tmp, err := os.CreateTemp("", "sklp-remote-cas-*")
	if err != nil {
		return fmt.Errorf("temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := io.Copy(tmp, rc); err != nil {
		tmp.Close()
		return fmt.Errorf("copy: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	got, err := cfg.CAS.PutFile(tmpName)
	if err != nil {
		return fmt.Errorf("put local: %w", err)
	}
	if got != digest {
		return fmt.Errorf("digest mismatch: remote %s, local %s", digest, got)
	}
	return nil
}

// pushToRemote runs in a goroutine: uploads every CAS object the
// remote does not yet have, then the result entry (logs first,
// result.json last — written by the transport layer for atomicity).
// Failures are logged to Stderr but never propagate as a step failure.
func pushToRemote(
	ctx context.Context,
	cfg CIExecConfig,
	stepName, stepKey string,
	res CIStepCacheResult,
	stdout, stderr []byte,
) {
	defer cfg.pushWG.Done()
	defer func() {
		if cfg.OnRemotePushDone != nil {
			cfg.OnRemotePushDone()
		}
	}()

	for _, o := range res.Outputs {
		ok, err := cfg.Remote.HasCAS(ctx, o.Digest)
		if err == nil && ok {
			continue
		}
		f, err := os.Open(cfg.CAS.pathFor(o.Digest))
		if err != nil {
			logRemoteFailure(cfg, stepName, fmt.Sprintf("open local cas %s: %v", short(o.Digest), err))
			continue
		}
		st, err := f.Stat()
		if err != nil {
			f.Close()
			logRemoteFailure(cfg, stepName, fmt.Sprintf("stat local cas %s: %v", short(o.Digest), err))
			continue
		}
		if err := cfg.Remote.PutCAS(ctx, o.Digest, f, st.Size()); err != nil {
			f.Close()
			logRemoteFailure(cfg, stepName, fmt.Sprintf("put cas %s: %v", short(o.Digest), err))
			continue
		}
		f.Close()
	}

	remoteOutputs := make([]RemoteCacheOutput, 0, len(res.Outputs))
	for _, o := range res.Outputs {
		remoteOutputs = append(remoteOutputs, RemoteCacheOutput{Path: o.Path, Digest: o.Digest})
	}
	blob := &RemoteStepResult{
		Name:       res.Name,
		ExitCode:   res.ExitCode,
		Status:     res.Status,
		DurationMS: res.DurationMS,
		Outputs:    remoteOutputs,
		Stdout:     stdout,
		Stderr:     stderr,
	}
	if err := cfg.Remote.PutResult(ctx, stepKey, blob); err != nil {
		logRemoteFailure(cfg, stepName, fmt.Sprintf("put result: %v", err))
		return
	}
	notifyRemote(cfg, stepName, "push_ok")
}

// drainRemotePushes waits for every in-flight upload to finish or the
// configured timeout to elapse, whichever comes first. A hung S3
// connection must not deadlock the run, so the timeout is a hard cap.
func drainRemotePushes(cfg CIExecConfig, wg *sync.WaitGroup) {
	timeout := cfg.RemotePushTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		if cfg.Stderr != nil {
			fmt.Fprintf(cfg.Stderr, "[sklp] remote cache: drain timed out after %s; uploads continue in background\n", timeout)
		}
	}
}

func logRemoteFailure(cfg CIExecConfig, stepName, msg string) {
	if cfg.Stderr != nil {
		fmt.Fprintf(cfg.Stderr, "[sklp] remote cache: step %q upload failed: %s\n", stepName, msg)
	}
	notifyRemote(cfg, stepName, "push_fail")
}

func notifyRemote(cfg CIExecConfig, stepName, event string) {
	if cfg.OnRemoteEvent != nil {
		cfg.OnRemoteEvent(stepName, event)
	}
}

func short(digest string) string {
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}
