// Package objectstore provides TLS-only S3 access shared by audit and DR.
package objectstore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type Store struct {
	Client *minio.Client
	Bucket string
	Prefix string
}

func New(endpoint, bucket, prefix string) (*Store, error) {
	if !strings.HasPrefix(endpoint, "https://") || bucket == "" {
		return nil, fmt.Errorf("S3 HTTPS endpoint and bucket required")
	}
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "us-east-1"
	}
	c, e := minio.New(strings.TrimPrefix(endpoint, "https://"), &minio.Options{Secure: true, Region: region, Creds: credentials.NewEnvAWS()})
	return &Store{c, bucket, strings.Trim(prefix, "/")}, e
}
func (s *Store) key(k string) string {
	if s.Prefix == "" {
		return k
	}
	return s.Prefix + "/" + k
}
func (s *Store) Put(ctx context.Context, key string, b []byte, contentType string) error {
	opts := minio.PutObjectOptions{ContentType: contentType, DisableMultipart: true, SendContentMd5: true}
	// Bucket-default retention applies without per-request bypass permissions.
	_, err := s.Client.PutObject(ctx, s.Bucket, s.key(key), bytes.NewReader(b), int64(len(b)), opts)
	return err
}
func (s *Store) Get(ctx context.Context, key string) ([]byte, error) {
	o, e := s.Client.GetObject(ctx, s.Bucket, s.key(key), minio.GetObjectOptions{VersionID: os.Getenv("S3_VERSION_ID")})
	if e != nil {
		return nil, e
	}
	defer o.Close()
	b, e := io.ReadAll(io.LimitReader(o, 256<<20+1))
	if len(b) > 256<<20 {
		return nil, fmt.Errorf("archive exceeds 256 MiB")
	}
	return b, e
}
