package objstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type Object struct {
	Key      string
	Size     int64
	Modified time.Time
}

type Store interface {
	List(ctx context.Context, prefix string) ([]Object, error)
	Open(ctx context.Context, key string) (io.ReadCloser, error)
}

type S3Config struct {
	Endpoint  string
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
}

type S3 struct {
	client *s3.Client
	bucket string
}

func NewS3(cfg S3Config) (*S3, error) {
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	endpoint := cfg.Endpoint
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	client := s3.NewFromConfig(aws.Config{
		Region:      cfg.Region,
		Credentials: credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
	}, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(strings.TrimSuffix(endpoint, "/"))
		o.UsePathStyle = true
	})
	return &S3{client: client, bucket: cfg.Bucket}, nil
}

func (s *S3) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	p := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket:    aws.String(s.bucket),
		Prefix:    aws.String(prefix),
		Delimiter: aws.String("/"),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list s3://%s/%s: %w", s.bucket, prefix, err)
		}
		for _, o := range page.Contents {
			out = append(out, Object{Key: aws.ToString(o.Key), Size: aws.ToInt64(o.Size), Modified: aws.ToTime(o.LastModified)})
		}
	}
	return out, nil
}

func (s *S3) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return nil, fmt.Errorf("get s3://%s/%s: %w", s.bucket, key, err)
	}
	return obj.Body, nil
}

// PartSize bounds what an upload holds in memory: one part at a time.
const PartSize = 16 << 20

var ErrEmpty = errors.New("nothing to upload")

// Put streams r to key as a multipart upload. A read error or an empty stream
// aborts it, so a failed producer never leaves an object behind.
func (s *S3) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	loc := fmt.Sprintf("s3://%s/%s", s.bucket, key)
	up, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return 0, fmt.Errorf("start upload %s: %w", loc, err)
	}
	var total int64
	abort := func(cause error) (int64, error) {
		_, _ = s.client.AbortMultipartUpload(context.WithoutCancel(ctx), &s3.AbortMultipartUploadInput{
			Bucket: aws.String(s.bucket), Key: aws.String(key), UploadId: up.UploadId,
		})
		return total, cause
	}
	var parts []types.CompletedPart
	buf := make([]byte, PartSize)
	for n := int32(1); ; n++ {
		m, rerr := io.ReadFull(r, buf)
		last := rerr == io.EOF || rerr == io.ErrUnexpectedEOF
		if rerr != nil && !last {
			return abort(rerr)
		}
		if m > 0 {
			out, err := s.client.UploadPart(ctx, &s3.UploadPartInput{
				Bucket: aws.String(s.bucket), Key: aws.String(key), UploadId: up.UploadId,
				PartNumber: aws.Int32(n), Body: bytes.NewReader(buf[:m]),
			})
			if err != nil {
				return abort(fmt.Errorf("upload part %d of %s: %w", n, loc, err))
			}
			parts = append(parts, types.CompletedPart{ETag: out.ETag, PartNumber: aws.Int32(n)})
			total += int64(m)
		}
		if last {
			break
		}
	}
	if total == 0 {
		return abort(ErrEmpty)
	}
	if _, err := s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key), UploadId: up.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	}); err != nil {
		return abort(fmt.Errorf("complete upload %s: %w", loc, err))
	}
	return total, nil
}

// Dir serves a local directory as a store; prefixes behave like S3's
// non-recursive listing, so tests and local drills see what S3 would return.
type Dir string

func (d Dir) List(_ context.Context, prefix string) ([]Object, error) {
	dir := filepath.Join(string(d), filepath.Dir(prefix+"x"))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []Object
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		rel, _ := filepath.Rel(string(d), filepath.Join(dir, e.Name()))
		key := filepath.ToSlash(rel)
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return nil, err
		}
		out = append(out, Object{Key: key, Size: info.Size(), Modified: info.ModTime()})
	}
	return out, nil
}

func (d Dir) Open(_ context.Context, key string) (io.ReadCloser, error) {
	return os.Open(filepath.Join(string(d), filepath.FromSlash(key)))
}
