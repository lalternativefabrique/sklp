package objstore

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
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
