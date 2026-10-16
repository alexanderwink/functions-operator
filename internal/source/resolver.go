// Package source resolves a Function's bootstrap binaries in S3.
package source

import (
	"context"
	"errors"
	"fmt"

	"github.com/minio/minio-go/v7"

	"github.com/alexanderwink/functions-operator/internal/fetcher"
)

// ErrObjectMissing is returned when a bootstrap object does not exist.
var ErrObjectMissing = errors.New("object missing")

// Resolved is the concrete source of a function.
type Resolved struct {
	Version string
	Prefix  string
	// ETags maps architecture to normalised (unquoted) ETag.
	ETags map[string]string
}

// Resolver looks up bootstrap objects.
type Resolver interface {
	Resolve(ctx context.Context, bucket, key, version string, archs []string) (Resolved, error)
}

type s3Resolver struct{ c *minio.Client }

// NewS3Resolver returns a Resolver that stats objects with c.
func NewS3Resolver(c *minio.Client) Resolver { return &s3Resolver{c: c} }

func (s *s3Resolver) Resolve(ctx context.Context, bucket, key, version string, archs []string) (Resolved, error) {
	prefix := key + "/" + version
	res := Resolved{Version: version, Prefix: prefix, ETags: map[string]string{}}
	for _, arch := range archs {
		obj := fetcher.ObjectFor(prefix, arch)
		st, err := s.c.StatObject(ctx, bucket, obj, minio.StatObjectOptions{})
		if err != nil {
			if minio.ToErrorResponse(err).Code == "NoSuchKey" {
				return Resolved{}, fmt.Errorf("%w: %s/%s", ErrObjectMissing, bucket, obj)
			}
			return Resolved{}, fmt.Errorf("stat %s/%s: %w", bucket, obj, err)
		}
		res.ETags[arch] = fetcher.NormalizeETag(st.ETag)
	}
	return res, nil
}
