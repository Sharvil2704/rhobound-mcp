// SPDX-License-Identifier: Apache-2.0

package objstore_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/objstore"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/objstore/s3fake"
)

var testCreds = objstore.Credentials{AccessKey: "AKIAIOSFODNN7EXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}

func fakeBucket(t *testing.T, page int) (*s3fake.Server, objstore.S3) {
	t.Helper()
	srv := &s3fake.Server{Bucket: "traces", Creds: testCreds, PageSize: page}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return srv, objstore.S3{Bucket: "traces", Region: "us-east-1", Endpoint: ts.URL, Creds: testCreds, Client: ts.Client()}
}

// The documented SigV4 example (S3 GET Object, "Examples: Signature Calculations"): the request for test.txt with
// Range bytes=0-9 at 20130524T000000Z by the example keys signs as f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41.
func TestS3SignsTheDocumentedExample(t *testing.T) {
	s := objstore.S3{Bucket: "examplebucket", Region: "us-east-1", Creds: testCreds,
		Now: func() time.Time { return time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC) }}
	req, _ := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	req.Header.Set("Range", "bytes=0-9")
	objstore.Sign(s, req)
	auth := req.Header.Get("Authorization")
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if auth != want {
		t.Fatalf("Authorization:\n got %s\nwant %s", auth, want)
	}
	// the fake's verifier agrees with the signer
	sig, err := objstore.SignatureOf(req, testCreds.SecretKey)
	if err != nil || !strings.HasSuffix(auth, "Signature="+sig) {
		t.Fatalf("SignatureOf: %s %v", sig, err)
	}
}

func TestS3ListsPagesOpensAndHeads(t *testing.T) {
	srv, s := fakeBucket(t, 3)
	keys := []string{
		"env=prod/year=2026/month=10/day=26/hour=12/minute=00/traces_a.json.gz",
		"env=prod/year=2026/month=10/day=26/hour=12/minute=00/traces_b.json.gz",
		"env=prod/year=2026/month=10/day=26/hour=12/minute=00/traces_c&d.json.gz", // XML-escaped in the listing
		"env=prod/year=2026/month=10/day=26/hour=12/minute=00/traces_e.json.gz",
		"env=prod/year=2026/month=10/day=26/hour=12/minute=01/traces_f.json.gz",
		"env=prod2/year=2026/month=10/day=26/hour=12/minute=00/traces_g.json.gz",
	}
	for i, k := range keys {
		srv.Put(k, []byte(fmt.Sprintf("object %d", i)))
	}
	ctx := context.Background()
	got, err := s.List(ctx, "env=prod/year=2026/month=10/day=26/hour=12/minute=00/")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[2].Key != keys[2] || got[0].Size != 8 || got[0].ETag == "" {
		t.Fatalf("minute listing (paged by 3): %+v", got)
	}
	if got, _ := s.List(ctx, "env=prod/"); len(got) != 5 {
		t.Fatalf("env=prod/ lists %d, want 5", len(got))
	}
	if got, err := s.List(ctx, "env=none/"); err != nil || len(got) != 0 {
		t.Fatalf("empty prefix: %v %v", got, err)
	}
	b, err := s.Head(ctx, keys[2], 3)
	if err != nil || string(b) != "obj" {
		t.Fatalf("head %q %v", b, err)
	}
	if b, err := s.Head(ctx, keys[2], 100); err != nil || string(b) != "object 2" {
		t.Fatalf("head past the end %q %v", b, err)
	}
	rc, err := s.Open(ctx, keys[4])
	if err != nil {
		t.Fatal(err)
	}
	all, _ := io.ReadAll(rc)
	rc.Close()
	if string(all) != "object 4" {
		t.Fatalf("open read %q", all)
	}
	if _, err := s.Open(ctx, "env=prod/nothing.json.gz"); !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("missing key: %v, want ErrNotFound", err)
	}
	if _, err := s.Head(ctx, "env=prod/nothing.json.gz", 4); !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("missing key head: %v, want ErrNotFound", err)
	}
	srv.Put("env=prod/empty.json.gz", nil)
	if b, err := s.Head(ctx, "env=prod/empty.json.gz", 4); err != nil || len(b) != 0 {
		t.Fatalf("empty object head: %q %v", b, err)
	}
}

func TestS3RetriesAndRefusesBadCredentials(t *testing.T) {
	srv, s := fakeBucket(t, 0)
	srv.Put("k/a.json.gz", []byte("hello"))
	ctx := context.Background()
	srv.FailOnce("k/a.json.gz", http.StatusServiceUnavailable)
	before := srv.Requests()
	rc, err := s.Open(ctx, "k/a.json.gz")
	if err != nil {
		t.Fatalf("after a 503: %v", err)
	}
	rc.Close()
	if srv.Requests()-before != 2 {
		t.Errorf("requests for one get with one 503: %d, want 2", srv.Requests()-before)
	}
	wrong := s
	wrong.Creds.SecretKey = "not-the-secret"
	wrong.Retries = 1
	if _, err := wrong.Open(ctx, "k/a.json.gz"); err == nil || !strings.Contains(err.Error(), "SignatureDoesNotMatch") {
		t.Errorf("wrong secret: %v", err)
	}
	wrong.Creds = objstore.Credentials{AccessKey: "AKIAOTHER", SecretKey: testCreds.SecretKey}
	if _, err := wrong.List(ctx, ""); err == nil || !strings.Contains(err.Error(), "SignatureDoesNotMatch") {
		t.Errorf("wrong access key: %v", err)
	}
	// a session token is signed too
	st := s
	st.Creds.SessionToken = "FwoGZXIvYXdzEBYaD"
	if rc, err := st.Open(ctx, "k/a.json.gz"); err != nil {
		t.Errorf("with a session token: %v", err)
	} else {
		rc.Close()
	}
	// the context's deadline ends the retries
	srv.FailOnce("k/a.json.gz", http.StatusBadGateway)
	srv.FailOnce("", http.StatusBadGateway)
	cctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := s.List(cctx, "k/"); err == nil {
		t.Error("a 502 then a timed-out retry should fail")
	}
}

func TestLoopbackOrHTTPS(t *testing.T) {
	for in, want := range map[string]bool{
		"https://s3.example.com": true, "http://127.0.0.1:9000": true, "http://localhost:9000": true, "http://[::1]:9000": true,
		"http://10.0.0.5:9000": false, "ftp://x": false, "https://s3.example.com/bucket": false, "": false, "https://x?y": false,
	} {
		if got := objstore.LoopbackOrHTTPS(in); got != want {
			t.Errorf("%q: %v, want %v", in, got, want)
		}
	}
}
