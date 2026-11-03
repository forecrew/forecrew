package evidence

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/forecrew/forecrew/internal/config"
)

// PartSize is the multipart part size of uploads. It bounds the memory of an
// upload (the client buffers a part per upload thread); the client's default
// is far larger.
const PartSize = 16 << 20

// ErrUnknownSize is returned by S3.Put for a size below zero.
var ErrUnknownSize = errors.New("evidence: object size must be known")

// S3 is a Store on an S3-compatible bucket.
type S3 struct {
	client *minio.Client
	bucket string
}

var _ Store = (*S3)(nil)

// NewS3 connects to the bucket of cfg, which config.Parse has validated.
// It does not contact the server: a wrong endpoint or key shows on first use.
// The keys are only handed to the client and never appear in errors.
func NewS3(cfg *config.Evidence) (*S3, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Host == "" {
		return nil, errors.New("evidence endpoint is not a URL")
	}
	lookup := minio.BucketLookupDNS
	if cfg.PathStyle {
		lookup = minio.BucketLookupPath
	}
	c, err := minio.New(u.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		Secure:       u.Scheme == "https",
		Region:       cfg.Region,
		BucketLookup: lookup,
	})
	if err != nil {
		return nil, fmt.Errorf("evidence client: %w", err)
	}
	return &S3{client: c, bucket: cfg.Bucket}, nil
}

// Put implements Store.
func (s *S3) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	if size < 0 {
		return ErrUnknownSize
	}
	_, err := s.client.PutObject(ctx, s.bucket, key, r, size, minio.PutObjectOptions{ContentType: contentType, PartSize: PartSize})
	if err != nil {
		return fmt.Errorf("put evidence: %w", err)
	}
	return nil
}

// Get implements Store.
func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("get evidence: %w", err)
	}
	// GetObject is lazy: Stat sends the request so a missing object shows now.
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		if minio.ToErrorResponse(err).Code == minio.NoSuchKey {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get evidence: %w", err)
	}
	return obj, nil
}

// Delete implements Store.
func (s *S3) Delete(ctx context.Context, key string) error {
	if err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("delete evidence: %w", err)
	}
	return nil
}
