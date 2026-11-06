// SPDX-License-Identifier: Apache-2.0

// Package s3fake is a fake S3 endpoint for tests and dry runs: path-style GET of objects (with ranges) and
// ListObjectsV2 with paging, over one bucket held in memory or loaded from a folder, every request verified
// against Signature Version 4 with the credentials it was given. It serves nothing else and stores nothing.
package s3fake

import (
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/objstore"
)

// Server is the fake. Set Bucket and Creds; Put objects or Load a folder; serve ServeHTTP.
type Server struct {
	Bucket   string
	Creds    objstore.Credentials
	PageSize int // keys per listing page; 0 means 1,000

	mu       sync.Mutex
	objs     map[string][]byte
	failOnce map[string]int // path -> status to answer once
	requests int
}

// Put stores an object.
func (s *Server) Put(key string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.objs == nil {
		s.objs = map[string][]byte{}
	}
	s.objs[key] = data
}

// Load puts every regular file under root as the object whose key is its path relative to root, with "/".
func (s *Server) Load(root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		s.Put(filepath.ToSlash(rel), b)
		return nil
	})
}

// FailOnce makes the next request for the path (a key, or "" for listings) answer the status once.
func (s *Server) FailOnce(key string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failOnce == nil {
		s.failOnce = map[string]int{}
	}
	s.failOnce[key] = status
}

// Requests counts the requests served.
func (s *Server) Requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

type xmlError struct {
	XMLName xml.Name `xml:"Error"`
	Code    string   `xml:"Code"`
	Message string   `xml:"Message"`
}

func fail(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	b, _ := xml.Marshal(xmlError{Code: code, Message: msg})
	w.Write(b)
}

// ServeHTTP answers one request.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests++
	s.mu.Unlock()
	if r.Method != http.MethodGet {
		fail(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "the fake serves GET only")
		return
	}
	sig, err := objstore.SignatureOf(r, s.Creds.SecretKey)
	if err != nil {
		fail(w, http.StatusForbidden, "AccessDenied", err.Error())
		return
	}
	auth := r.Header.Get("Authorization")
	if !strings.Contains(auth, "Credential="+s.Creds.AccessKey+"/") || !strings.HasSuffix(auth, "Signature="+sig) {
		fail(w, http.StatusForbidden, "SignatureDoesNotMatch", "the request signature does not match")
		return
	}
	if r.Header.Get("x-amz-content-sha256") == "" {
		fail(w, http.StatusBadRequest, "InvalidRequest", "x-amz-content-sha256 is required")
		return
	}
	// path-style: /bucket or /bucket/key
	p := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(p, "/")
	if bucket != s.Bucket {
		fail(w, http.StatusNotFound, "NoSuchBucket", "no such bucket")
		return
	}
	s.mu.Lock()
	if st, ok := s.failOnce[key]; ok {
		delete(s.failOnce, key)
		s.mu.Unlock()
		fail(w, st, "InternalError", "injected")
		return
	}
	s.mu.Unlock()
	if key == "" {
		s.list(w, r)
		return
	}
	s.object(w, r, key)
}

type contents struct {
	Key  string `xml:"Key"`
	Size int64  `xml:"Size"`
	ETag string `xml:"ETag"`
}

type listResult struct {
	XMLName               xml.Name   `xml:"ListBucketResult"`
	Name                  string     `xml:"Name"`
	Prefix                string     `xml:"Prefix"`
	KeyCount              int        `xml:"KeyCount"`
	MaxKeys               int        `xml:"MaxKeys"`
	IsTruncated           bool       `xml:"IsTruncated"`
	NextContinuationToken string     `xml:"NextContinuationToken,omitempty"`
	Contents              []contents `xml:"Contents"`
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("list-type") != "2" {
		fail(w, http.StatusBadRequest, "InvalidArgument", "list-type=2 only")
		return
	}
	prefix := q.Get("prefix")
	size := s.PageSize
	if size <= 0 {
		size = 1000
	}
	if mk, err := strconv.Atoi(q.Get("max-keys")); err == nil && mk > 0 && mk < size {
		size = mk
	}
	after := ""
	if tok := q.Get("continuation-token"); tok != "" {
		b, err := base64.StdEncoding.DecodeString(tok)
		if err != nil {
			fail(w, http.StatusBadRequest, "InvalidArgument", "bad continuation token")
			return
		}
		after = string(b)
	}
	s.mu.Lock()
	var keys []string
	for k := range s.objs {
		if strings.HasPrefix(k, prefix) && k > after {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	res := listResult{Name: s.Bucket, Prefix: prefix, MaxKeys: size}
	for i, k := range keys {
		if i == size {
			res.IsTruncated = true
			res.NextContinuationToken = base64.StdEncoding.EncodeToString([]byte(keys[i-1]))
			break
		}
		res.Contents = append(res.Contents, contents{Key: k, Size: int64(len(s.objs[k])), ETag: `"` + fmt.Sprintf("%x", len(s.objs[k])) + `"`})
	}
	s.mu.Unlock()
	res.KeyCount = len(res.Contents)
	w.Header().Set("Content-Type", "application/xml")
	b, _ := xml.Marshal(res)
	w.Write(append([]byte(xml.Header), b...))
}

func (s *Server) object(w http.ResponseWriter, r *http.Request, key string) {
	s.mu.Lock()
	b, ok := s.objs[key]
	s.mu.Unlock()
	if !ok {
		fail(w, http.StatusNotFound, "NoSuchKey", "the specified key does not exist")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	if rng := r.Header.Get("Range"); rng != "" {
		lo, hi, ok := parseRange(rng, int64(len(b)))
		if !ok {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(b)))
			fail(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", "the requested range is not satisfiable")
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", lo, hi, len(b)))
		w.Header().Set("Content-Length", strconv.Itoa(int(hi-lo+1)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(b[lo : hi+1])
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.Write(b)
}

// parseRange reads "bytes=lo-hi" (hi may be absent) against a size.
func parseRange(h string, size int64) (int64, int64, bool) {
	spec, ok := strings.CutPrefix(h, "bytes=")
	if !ok {
		return 0, 0, false
	}
	los, his, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, 0, false
	}
	lo, err := strconv.ParseInt(los, 10, 64)
	if err != nil || lo < 0 || lo >= size {
		return 0, 0, false
	}
	hi := size - 1
	if his != "" {
		if hi, err = strconv.ParseInt(his, 10, 64); err != nil || hi < lo {
			return 0, 0, false
		}
		if hi >= size {
			hi = size - 1
		}
	}
	return lo, hi, true
}
