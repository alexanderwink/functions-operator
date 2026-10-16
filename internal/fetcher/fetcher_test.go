package fetcher

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

const (
	testETag = "abc123-2" // multipart-style, not an MD5
	testBody = "#!/bin/sh\necho hi\n"
)

func stubS3(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bkt/f/v1/linux-amd64/bootstrap" {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchKey</Code><Message>nope</Message></Error>`))
			return
		}
		if im := strings.Trim(r.Header.Get("If-Match"), `"`); im != "" && im != testETag {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusPreconditionFailed)
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>PreconditionFailed</Code><Message>At least one of the pre-conditions you specified did not hold</Message></Error>`))
			return
		}
		w.Header().Set("ETag", `"`+testETag+`"`)
		w.Header().Set("Content-Length", "18")
		w.Header().Set("Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT")
		_, _ = w.Write([]byte(testBody))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func client(t *testing.T, srv *httptest.Server) *minio.Client {
	t.Helper()
	c, err := minio.New(strings.TrimPrefix(srv.URL, "http://"), &minio.Options{
		Creds:  credentials.NewStaticV4("k", "s", ""),
		Region: "us-east-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestObjectFor(t *testing.T) {
	if got := ObjectFor("f/v1", "arm64"); got != "f/v1/linux-arm64/bootstrap" {
		t.Fatalf("got %q", got)
	}
}

func TestFetchWritesExecutable(t *testing.T) {
	c := client(t, stubS3(t))
	for _, etag := range []string{testETag, `"` + testETag + `"`} {
		dest := filepath.Join(t.TempDir(), "sub", "bootstrap")
		if err := Fetch(context.Background(), c, "bkt", "f/v1/linux-amd64/bootstrap", etag, dest); err != nil {
			t.Fatalf("etag %q: %v", etag, err)
		}
		st, err := os.Stat(dest)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o755 {
			t.Fatalf("mode %v", st.Mode())
		}
		b, _ := os.ReadFile(dest)
		if string(b) != testBody {
			t.Fatalf("content %q", b)
		}
		if _, err := os.Stat(dest + ".tmp"); !os.IsNotExist(err) {
			t.Fatalf("tmp left behind: %v", err)
		}
	}
}

func TestFetchETagMismatch(t *testing.T) {
	c := client(t, stubS3(t))
	dest := filepath.Join(t.TempDir(), "bootstrap")
	err := Fetch(context.Background(), c, "bkt", "f/v1/linux-amd64/bootstrap", "other", dest)
	if !errors.Is(err, ErrETagMismatch) {
		t.Fatalf("want ErrETagMismatch, got %v", err)
	}
	for _, p := range []string{dest, dest + ".tmp"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s should not exist: %v", p, err)
		}
	}
}

func TestFetchOtherErrorNotMismatch(t *testing.T) {
	c := client(t, stubS3(t))
	dest := filepath.Join(t.TempDir(), "bootstrap")
	err := Fetch(context.Background(), c, "bkt", "missing", testETag, dest)
	if err == nil || errors.Is(err, ErrETagMismatch) {
		t.Fatalf("got %v", err)
	}
}

func runEnv(t *testing.T, env map[string]string, arch string) (int, string, string) {
	t.Helper()
	var stderr bytes.Buffer
	code := Run(context.Background(), func(k string) string { return env[k] }, arch, &stderr)
	return code, stderr.String(), env["FETCH_TERMINATION_LOG"]
}

func baseEnv(t *testing.T, srv *httptest.Server) map[string]string {
	d := t.TempDir()
	return map[string]string{
		"S3_ENDPOINT":           strings.TrimPrefix(srv.URL, "http://"),
		"S3_ACCESS_KEY":         "k",
		"S3_SECRET_KEY":         "s",
		"S3_USE_SSL":            "false",
		"FETCH_BUCKET":          "bkt",
		"FETCH_PREFIX":          "f/v1",
		"FETCH_ETAG_AMD64":      testETag,
		"FETCH_DEST":            filepath.Join(d, "bootstrap"),
		"FETCH_TERMINATION_LOG": filepath.Join(d, "term"),
	}
}

func TestRunSuccess(t *testing.T) {
	env := baseEnv(t, stubS3(t))
	code, stderr, _ := runEnv(t, env, "amd64")
	if code != 0 {
		t.Fatalf("code %d: %s", code, stderr)
	}
	if _, err := os.Stat(env["FETCH_DEST"]); err != nil {
		t.Fatal(err)
	}
}

func TestRunMismatchExit3(t *testing.T) {
	env := baseEnv(t, stubS3(t))
	env["FETCH_ETAG_AMD64"] = "wrong"
	code, _, term := runEnv(t, env, "amd64")
	if code != 3 {
		t.Fatalf("code %d", code)
	}
	b, _ := os.ReadFile(term)
	if string(b) != "etag-mismatch" {
		t.Fatalf("termination log %q", b)
	}
}

func TestRunMissingArchETag(t *testing.T) {
	env := baseEnv(t, stubS3(t))
	code, stderr, _ := runEnv(t, env, "arm64")
	if code != 1 || !strings.Contains(stderr, "FETCH_ETAG_ARM64") {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	if _, err := os.Stat(env["FETCH_DEST"]); !os.IsNotExist(err) {
		t.Fatal("dest must not exist")
	}
}

func TestParseUseSSL(t *testing.T) {
	for in, want := range map[string]bool{"true": true, "false": false, "1": true, "0": false, "TRUE": true, "False": false} {
		if got, err := ParseUseSSL(in); err != nil || got != want {
			t.Errorf("ParseUseSSL(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "yes", "on", "tru"} {
		if _, err := ParseUseSSL(in); err == nil {
			t.Errorf("ParseUseSSL(%q) succeeded; want error", in)
		}
	}
}

func TestRunUseSSL(t *testing.T) {
	srv := stubS3(t)
	for _, tc := range []struct {
		val, wantErr string
	}{
		{"", "S3_USE_SSL"},
		{"yes", `useSSL "yes" is not a boolean`},
	} {
		env := baseEnv(t, srv)
		env["S3_USE_SSL"] = tc.val
		code, stderr, _ := runEnv(t, env, "amd64")
		if code != 1 || !strings.Contains(stderr, tc.wantErr) {
			t.Errorf("S3_USE_SSL=%q: code %d stderr %q; want 1 mentioning %q", tc.val, code, stderr, tc.wantErr)
		}
	}
	env := baseEnv(t, srv)
	env["S3_USE_SSL"] = "0"
	if code, stderr, _ := runEnv(t, env, "amd64"); code != 0 {
		t.Errorf("S3_USE_SSL=0: code %d: %s", code, stderr)
	}
}

func TestRunMissingConfig(t *testing.T) {
	code, stderr, _ := runEnv(t, map[string]string{}, "amd64")
	if code != 1 || stderr == "" {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
}
