package main

import (
	"strings"
	"testing"
)

func TestS3Options(t *testing.T) {
	base := func() map[string][]byte {
		return map[string][]byte{
			"endpoint": []byte("minio:9000"), "accessKey": []byte("a"), "secretKey": []byte("s"), "useSSL": []byte("false"),
		}
	}
	for val, want := range map[string]bool{"true": true, "false": false, "1": true} {
		d := base()
		d["useSSL"] = []byte(val)
		ep, opts, err := s3Options(d)
		if err != nil || ep != "minio:9000" || opts.Secure != want {
			t.Errorf("useSSL=%q: endpoint %q secure %v err %v; want secure %v", val, ep, opts != nil && opts.Secure, err, want)
		}
	}

	d := base()
	delete(d, "useSSL")
	if _, _, err := s3Options(d); err == nil || !strings.Contains(err.Error(), "useSSL") {
		t.Errorf("missing useSSL: err = %v; want a missing-key error naming useSSL", err)
	}
	d = base()
	d["useSSL"] = []byte("yes")
	if _, _, err := s3Options(d); err == nil || !strings.Contains(err.Error(), "not a boolean") {
		t.Errorf("useSSL=yes: err = %v; want a parse error", err)
	}
}
