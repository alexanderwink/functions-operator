package source

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

func stub(t *testing.T, objects map[string]string) *minio.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		etag, ok := objects[r.URL.Path]
		if !ok {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchKey</Code><Message>nope</Message></Error>`))
			return
		}
		w.Header().Set("ETag", `"`+etag+`"`)
		w.Header().Set("Content-Length", "1")
		w.Header().Set("Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT")
	}))
	t.Cleanup(srv.Close)
	c, err := minio.New(strings.TrimPrefix(srv.URL, "http://"), &minio.Options{
		Creds:  credentials.NewStaticV4("k", "s", ""),
		Region: "us-east-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestResolverAllArchs(t *testing.T) {
	c := stub(t, map[string]string{
		"/bkt/f/v1/linux-amd64/bootstrap": "aaa",
		"/bkt/f/v1/linux-arm64/bootstrap": "bbb-2",
	})
	r, err := NewS3Resolver(c).Resolve(context.Background(), "bkt", "f", "v1", []string{"amd64", "arm64"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Prefix != "f/v1" || r.Version != "v1" {
		t.Fatalf("got %+v", r)
	}
	if r.ETags["amd64"] != "aaa" || r.ETags["arm64"] != "bbb-2" || len(r.ETags) != 2 {
		t.Fatalf("etags %v", r.ETags)
	}
}

func TestResolverMissingArch(t *testing.T) {
	c := stub(t, map[string]string{"/bkt/f/v1/linux-amd64/bootstrap": "aaa"})
	_, err := NewS3Resolver(c).Resolve(context.Background(), "bkt", "f", "v1", []string{"amd64", "arm64"})
	if !errors.Is(err, ErrObjectMissing) {
		t.Fatalf("want ErrObjectMissing, got %v", err)
	}
	if !strings.Contains(err.Error(), "linux-arm64") {
		t.Fatalf("message %q", err)
	}
}
