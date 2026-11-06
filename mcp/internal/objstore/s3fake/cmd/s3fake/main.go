// SPDX-License-Identifier: Apache-2.0

// Command s3fake serves a folder as one S3 bucket over plain HTTP on a loopback address, for dry runs of the s3
// store without AWS: `go run ./internal/objstore/s3fake/cmd/s3fake --root DIR --bucket NAME` from mcp/. Requests
// must be signed with the access key and secret it is given (SigV4), as a real bucket demands. It is a test tool:
// it never writes, and it is not a replacement for S3.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"

	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/objstore"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/objstore/s3fake"
)

func main() {
	root := flag.String("root", "", "the folder served as the bucket (keys are paths under it)")
	bucket := flag.String("bucket", "traces", "the bucket's name")
	listen := flag.String("listen", "127.0.0.1:0", "the address to listen on (loopback only)")
	access := flag.String("access-key", "AKIAFAKEFAKEFAKEFAKE", "the access key requests must be signed with")
	secret := flag.String("secret-key", "fake-secret-key-for-dry-runs-only", "its secret")
	page := flag.Int("page", 1000, "keys per listing page")
	flag.Parse()
	if *root == "" {
		fmt.Fprintln(os.Stderr, "usage: s3fake --root DIR [--bucket NAME] [--listen 127.0.0.1:PORT] [--access-key K --secret-key S] [--page N]")
		os.Exit(2)
	}
	srv := &s3fake.Server{Bucket: *bucket, Creds: objstore.Credentials{AccessKey: *access, SecretKey: *secret}, PageSize: *page}
	if err := srv.Load(*root); err != nil {
		log.Fatalf("s3fake: %v", err)
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("s3fake: %v", err)
	}
	if host, _, _ := net.SplitHostPort(ln.Addr().String()); host != "127.0.0.1" && host != "::1" {
		log.Fatalf("s3fake: listen on a loopback address only (credentials travel in the clear)")
	}
	fmt.Printf("s3fake: bucket %s from %s at http://%s/\n", *bucket, *root, ln.Addr())
	log.Fatal(http.Serve(ln, srv))
}
