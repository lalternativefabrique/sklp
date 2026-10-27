// Package remotecache is the S3-compatible remote-cache transport for
// the sklp engine. It mirrors the local per-step cache + content-
// addressed store (see internal/plan) to a shared bucket so a teammate
// or CI runner that executes the same plan hits `cached (remote)`
// instead of paying the full execution cost.
//
// Bucket layout (deliberately human-inspectable, identical across
// every supported backend — AWS S3, Scaleway Object Storage, R2,
// MinIO):
//
//	cas/<sha>                       — raw bytes of one content-addressed
//	                                  step output. Idempotent: a
//	                                  duplicate PUT against the same key
//	                                  is a no-op.
//	runs/<step-key>/result.json     — step result entry (status, exit
//	                                  code, duration, output manifest).
//	runs/<step-key>/stdout.log      — raw stdout captured at run time.
//	runs/<step-key>/stderr.log      — raw stderr captured at run time.
//
// PutResult writes result.json LAST so a partial upload is invisible
// to a concurrent GetResult: a reader either sees the full entry or
// reports a miss.
//
// Configuration (read once at process start):
//
//	SKLP_REMOTE_CACHE_URL     — endpoint URL (e.g. https://s3.fr-par.scw.cloud)
//	SKLP_REMOTE_CACHE_BUCKET  — bucket name
//	AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_REGION
//
// New returns (nil, nil) when SKLP_REMOTE_CACHE_URL / _BUCKET are
// unset — the "remote disabled" sentinel callers must handle.
package remotecache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// ErrNotFound is returned by GetCAS / GetResult when the requested key
// does not exist in the bucket. Callers treat it identically to a local
// cache miss.
var ErrNotFound = errors.New("remotecache: not found")

// OutputEntry pairs one declared output path with its CAS digest.
// Mirrors plan.CIStepCacheOutput but is duplicated here so internal/
// remotecache does not import internal/plan (the wire format is the
// authoritative contract; the local struct could evolve independently).
type OutputEntry struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
}

// StepResultBlob is the value type for the per-step result entry. It
// carries everything a remote consumer needs to materialise a cache hit
// without further round-trips beyond the CAS objects listed in Outputs.
type StepResultBlob struct {
	Name       string        `json:"name"`
	ExitCode   int           `json:"exit_code"`
	Status     string        `json:"status"`
	DurationMS int64         `json:"duration_ms"`
	Outputs    []OutputEntry `json:"outputs,omitempty"`
	Stdout     []byte        `json:"-"`
	Stderr     []byte        `json:"-"`
}

// RemoteStatus is what `sklp cache status` renders for the remote
// section. Endpoint / Bucket are surfaced verbatim; secrets are never
// included.
type RemoteStatus struct {
	Endpoint string
	Bucket   string
}

// RemoteCache is the transport-level interface every backend
// implements. The S3 implementation lives in this file; tests inject a
// fake to keep the suite hermetic.
type RemoteCache interface {
	GetCAS(ctx context.Context, sha string) (io.ReadCloser, error)
	PutCAS(ctx context.Context, sha string, r io.Reader, size int64) error
	GetResult(ctx context.Context, stepKey string) (*StepResultBlob, error)
	PutResult(ctx context.Context, stepKey string, blob *StepResultBlob) error
	HasCAS(ctx context.Context, sha string) (bool, error)
	Status() RemoteStatus
}

// New returns an S3-backed RemoteCache configured from the env, or
// (nil, nil) when SKLP_REMOTE_CACHE_URL / _BUCKET are unset. The
// (nil, nil) shape is the "remote disabled" sentinel — callers must
// nil-check the returned value before using it.
func New(ctx context.Context) (RemoteCache, error) {
	endpoint := os.Getenv("SKLP_REMOTE_CACHE_URL")
	bucket := os.Getenv("SKLP_REMOTE_CACHE_BUCKET")
	if endpoint == "" || bucket == "" {
		return nil, nil
	}
	region := os.Getenv("AWS_REGION")
	if region == "" {
		// Most S3-compatible providers ignore the region but the SDK
		// refuses to sign without one. Pick a safe default.
		region = "us-east-1"
	}
	access := os.Getenv("AWS_ACCESS_KEY_ID")
	secret := os.Getenv("AWS_SECRET_ACCESS_KEY")
	if access == "" || secret == "" {
		return nil, fmt.Errorf("remotecache: SKLP_REMOTE_CACHE_URL is set but AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY are missing")
	}

	cfg := awsv2.Config{
		Region:      region,
		Credentials: credentials.NewStaticCredentialsProvider(access, secret, ""),
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = awsv2.String(endpoint)
		o.UsePathStyle = true
	})
	return &s3Cache{
		client:   client,
		bucket:   bucket,
		endpoint: endpoint,
	}, nil
}

type s3Cache struct {
	client   *s3.Client
	bucket   string
	endpoint string
}

func (s *s3Cache) Status() RemoteStatus {
	return RemoteStatus{Endpoint: s.endpoint, Bucket: s.bucket}
}

func (s *s3Cache) GetCAS(ctx context.Context, sha string) (io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: awsv2.String(s.bucket),
		Key:    awsv2.String(casKey(sha)),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("remotecache: get cas %s: %w", sha, err)
	}
	return out.Body, nil
}

func (s *s3Cache) HasCAS(ctx context.Context, sha string) (bool, error) {
	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: awsv2.String(s.bucket),
		Key:    awsv2.String(casKey(sha)),
	})
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("remotecache: head cas %s: %w", sha, err)
	}
	return true, nil
}

func (s *s3Cache) PutCAS(ctx context.Context, sha string, r io.Reader, size int64) error {
	// CAS is content-addressed, so a HEAD-skip is a pure latency win;
	// it isn't a correctness gate (a duplicate PUT would be a no-op).
	if ok, _ := s.HasCAS(ctx, sha); ok {
		return nil
	}
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        awsv2.String(s.bucket),
		Key:           awsv2.String(casKey(sha)),
		Body:          r,
		ContentLength: awsv2.Int64(size),
	})
	if err != nil {
		return fmt.Errorf("remotecache: put cas %s: %w", sha, err)
	}
	return nil
}

func (s *s3Cache) GetResult(ctx context.Context, stepKey string) (*StepResultBlob, error) {
	resObj, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: awsv2.String(s.bucket),
		Key:    awsv2.String(resultKey(stepKey)),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("remotecache: get result %s: %w", stepKey, err)
	}
	defer resObj.Body.Close()
	resBytes, err := io.ReadAll(resObj.Body)
	if err != nil {
		return nil, fmt.Errorf("remotecache: read result %s: %w", stepKey, err)
	}
	var blob StepResultBlob
	if err := json.Unmarshal(resBytes, &blob); err != nil {
		return nil, fmt.Errorf("remotecache: decode result %s: %w", stepKey, err)
	}
	if b, err := s.getOptional(ctx, stdoutKey(stepKey)); err != nil {
		return nil, err
	} else {
		blob.Stdout = b
	}
	if b, err := s.getOptional(ctx, stderrKey(stepKey)); err != nil {
		return nil, err
	} else {
		blob.Stderr = b
	}
	return &blob, nil
}

func (s *s3Cache) PutResult(ctx context.Context, stepKey string, blob *StepResultBlob) error {
	if blob == nil {
		return errors.New("remotecache: nil blob")
	}
	// Write logs first; result.json last. A concurrent GetResult that
	// races with us will either see the full entry (result.json present
	// implies logs present) or report a miss.
	if err := s.putBytes(ctx, stdoutKey(stepKey), blob.Stdout); err != nil {
		return err
	}
	if err := s.putBytes(ctx, stderrKey(stepKey), blob.Stderr); err != nil {
		return err
	}
	resBytes, err := json.Marshal(blob)
	if err != nil {
		return fmt.Errorf("remotecache: encode result %s: %w", stepKey, err)
	}
	if err := s.putBytes(ctx, resultKey(stepKey), resBytes); err != nil {
		return err
	}
	return nil
}

func (s *s3Cache) putBytes(ctx context.Context, key string, b []byte) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        awsv2.String(s.bucket),
		Key:           awsv2.String(key),
		Body:          bytes.NewReader(b),
		ContentLength: awsv2.Int64(int64(len(b))),
	})
	if err != nil {
		return fmt.Errorf("remotecache: put %s: %w", key, err)
	}
	return nil
}

func (s *s3Cache) getOptional(ctx context.Context, key string) ([]byte, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: awsv2.String(s.bucket),
		Key:    awsv2.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("remotecache: get %s: %w", key, err)
	}
	defer out.Body.Close()
	return io.ReadAll(out.Body)
}

func casKey(sha string) string        { return "cas/" + sha }
func resultKey(stepKey string) string { return "runs/" + stepKey + "/result.json" }
func stdoutKey(stepKey string) string { return "runs/" + stepKey + "/stdout.log" }
func stderrKey(stepKey string) string { return "runs/" + stepKey + "/stderr.log" }

func isNotFound(err error) bool {
	var nsk *s3types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var nf *s3types.NotFound
	return errors.As(err, &nf)
}
