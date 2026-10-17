// Package fetcher downloads a function's bootstrap binary from S3-compatible
// storage, pinned to an exact object version by ETag.
package fetcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// ErrETagMismatch is returned when the object's ETag no longer matches the pinned one.
var ErrETagMismatch = errors.New("etag mismatch")

// ParseUseSSL parses the S3 Secret's useSSL value (strconv.ParseBool syntax).
// The key is required: an empty value is an error, as is any value ParseBool
// rejects. The operator and the fetcher both use it, so they cannot disagree.
func ParseUseSSL(v string) (bool, error) {
	if v == "" {
		return false, errors.New(`useSSL is required ("true" or "false")`)
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return false, fmt.Errorf(`useSSL %q is not a boolean ("true" or "false")`, v)
	}
	return b, nil
}

// ObjectFor returns the object key of the bootstrap for an architecture.
func ObjectFor(prefix, arch string) string {
	return prefix + "/linux-" + arch + "/bootstrap"
}

// NormalizeETag strips surrounding quotes. ETags are opaque strings (multipart
// ETags are not MD5), so no other interpretation is applied.
func NormalizeETag(etag string) string {
	return strings.Trim(strings.TrimSpace(etag), `"`)
}

// Fetch downloads bucket/object to dest, requiring that the object's ETag
// equals etag (quoted or not). On mismatch it returns ErrETagMismatch. dest is
// written via dest.tmp and renamed; no partial dest is left on error.
func Fetch(ctx context.Context, c *minio.Client, bucket, object, etag, dest string) (err error) {
	etag = NormalizeETag(etag)
	if etag == "" {
		return errors.New("empty etag")
	}
	opts := minio.GetObjectOptions{}
	if err := opts.SetMatchETag(etag); err != nil {
		return err
	}
	obj, err := c.GetObject(ctx, bucket, object, opts)
	if err != nil {
		return mapErr(err)
	}
	defer func() { _ = obj.Close() }()

	// GetObject is lazy; the first Stat/Read surfaces HTTP errors.
	if _, err := obj.Stat(); err != nil {
		return mapErr(err)
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp := dest + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if _, err = io.Copy(f, obj); err != nil {
		return mapErr(err)
	}
	if err = f.Chmod(0o755); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

func mapErr(err error) error {
	resp := minio.ToErrorResponse(err)
	if resp.StatusCode == 412 || resp.Code == "PreconditionFailed" {
		return fmt.Errorf("%w: %s", ErrETagMismatch, resp.Message)
	}
	return err
}

// Run executes the fetcher using env lookups; it returns the process exit code.
func Run(ctx context.Context, getenv func(string) string, arch string, stderr io.Writer) int {
	fail := func(format string, a ...any) int {
		_, _ = fmt.Fprintf(stderr, "fetcher: "+format+"\n", a...)
		return 1
	}
	var missing []string
	need := func(k string) string {
		v := getenv(k)
		if v == "" {
			missing = append(missing, k)
		}
		return v
	}
	endpoint, ak, sk := need("S3_ENDPOINT"), need("S3_ACCESS_KEY"), need("S3_SECRET_KEY")
	useSSLRaw := need("S3_USE_SSL")
	bucket, prefix := need("FETCH_BUCKET"), need("FETCH_PREFIX")
	etagVar := "FETCH_ETAG_" + strings.ToUpper(arch)
	etag := getenv(etagVar)
	if etag == "" {
		return fail("%s is not set: architecture %q is not configured for this function (scheduling error?)", etagVar, arch)
	}
	if len(missing) > 0 {
		return fail("missing required environment: %s", strings.Join(missing, ", "))
	}
	useSSL, err := ParseUseSSL(useSSLRaw)
	if err != nil {
		return fail("S3_USE_SSL (Secret %s key useSSL): %v", "functions-s3-read", err)
	}
	dest := getenv("FETCH_DEST")
	if dest == "" {
		dest = "/var/task/bootstrap"
	}
	termLog := getenv("FETCH_TERMINATION_LOG")
	if termLog == "" {
		termLog = "/dev/termination-log"
	}

	c, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(ak, sk, ""),
		Secure: useSSL,
		Region: "us-east-1", // avoid a GetBucketLocation round trip
	})
	if err != nil {
		return fail("creating S3 client: %v", err)
	}
	object := ObjectFor(prefix, arch)
	if err := Fetch(ctx, c, bucket, object, etag, dest); err != nil {
		if errors.Is(err, ErrETagMismatch) {
			_ = os.WriteFile(termLog, []byte("etag-mismatch"), 0o644)
			_, _ = fmt.Fprintf(stderr, "fetcher: %s/%s: %v\n", bucket, object, err)
			return 3
		}
		return fail("fetching %s/%s: %v", bucket, object, err)
	}
	return 0
}
