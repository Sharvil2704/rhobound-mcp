// SPDX-License-Identifier: Apache-2.0

package objstore

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Credentials are AWS credentials: an access key and its secret, and a session token for temporary ones.
type Credentials struct {
	AccessKey, SecretKey, SessionToken string
}

// CredentialsFromEnv reads the standard environment variables (AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY,
// AWS_SESSION_TOKEN), which Lambda sets for the function's role, and reports whether both keys are set. A role's
// credentials served by the container or instance metadata endpoints are not read: export them before a refit on
// ECS or EC2.
func CredentialsFromEnv() (Credentials, bool) {
	c := Credentials{AccessKey: os.Getenv("AWS_ACCESS_KEY_ID"), SecretKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		SessionToken: os.Getenv("AWS_SESSION_TOKEN")}
	return c, c.AccessKey != "" && c.SecretKey != ""
}

// RegionFromEnv is AWS_REGION, else AWS_DEFAULT_REGION.
func RegionFromEnv() string {
	if r := os.Getenv("AWS_REGION"); r != "" {
		return r
	}
	return os.Getenv("AWS_DEFAULT_REGION")
}

// S3 is a read-only client of one bucket over the S3 REST API, signed with Signature Version 4, with no dependency
// beyond the standard library. Without an Endpoint it addresses the bucket by its virtual-hosted name,
// https://BUCKET.s3.REGION.amazonaws.com; with one (an S3-compatible store, a test fake) it uses path-style
// requests, ENDPOINT/BUCKET/KEY. GET requests carry the hash of their empty payload, so a proxy that re-chunks
// bodies cannot break the signature. Listings page with max-keys and continuation tokens. A 5xx answer or a
// connection failure is retried a few times with a growing pause; a 404 is ErrNotFound.
type S3 struct {
	Bucket, Region string
	Endpoint       string
	Creds          Credentials
	// Client is the HTTP client; nil means http.DefaultClient. Deadlines come from the context.
	Client *http.Client
	// PageSize is max-keys per listing page; 0 means 1,000 (the API's own maximum).
	PageSize int
	// Retries is how many times a request is retried on a 5xx or a connection failure; 0 means 3.
	Retries int
	// Now is the clock for x-amz-date; nil means time.Now.
	Now func() time.Time
}

// Kind names the store in a capture manifest's source.
func (s S3) Kind() string { return "s3" }

func (s S3) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return http.DefaultClient
}

func (s S3) host() string {
	if s.Endpoint != "" {
		u, _ := url.Parse(s.Endpoint)
		return u.Host
	}
	return s.Bucket + ".s3." + s.Region + ".amazonaws.com"
}

// objectURL is the URL of a key (path-style under an endpoint, virtual-hosted otherwise), with the key's segments
// URI-encoded as SigV4 wants them.
func (s S3) objectURL(key string, query url.Values) string {
	path := "/" + encodePath(key)
	base := "https://" + s.host()
	if s.Endpoint != "" {
		base = strings.TrimSuffix(s.Endpoint, "/")
		path = "/" + encodePath(s.Bucket) + path
		if key == "" {
			path = "/" + encodePath(s.Bucket)
		}
	}
	u := base + path
	if q := canonicalQuery(query); q != "" {
		u += "?" + q
	}
	return u
}

// List lists every key under prefix, paging with continuation tokens, sorted by key.
func (s S3) List(ctx context.Context, prefix string) ([]Object, error) {
	size := s.PageSize
	if size <= 0 || size > 1000 {
		size = 1000
	}
	var out []Object
	token := ""
	for page := 0; ; page++ {
		q := url.Values{"list-type": {"2"}, "max-keys": {strconv.Itoa(size)}}
		if prefix != "" {
			q.Set("prefix", prefix)
		}
		if token != "" {
			q.Set("continuation-token", token)
		}
		body, _, err := s.get(ctx, s.objectURL("", q), "")
		if err != nil {
			return nil, fmt.Errorf("objstore: s3 list %q page %d: %w", prefix, page, err)
		}
		var res struct {
			IsTruncated           bool   `xml:"IsTruncated"`
			NextContinuationToken string `xml:"NextContinuationToken"`
			Contents              []struct {
				Key  string `xml:"Key"`
				Size int64  `xml:"Size"`
				ETag string `xml:"ETag"`
			} `xml:"Contents"`
		}
		if err := xml.Unmarshal(body, &res); err != nil {
			return nil, fmt.Errorf("objstore: s3 list %q: not a ListBucketResult: %w", prefix, err)
		}
		for _, c := range res.Contents {
			if !strings.HasPrefix(c.Key, prefix) {
				continue
			}
			out = append(out, Object{Key: c.Key, Size: c.Size, ETag: c.ETag})
		}
		if !res.IsTruncated || res.NextContinuationToken == "" {
			break
		}
		token = res.NextContinuationToken
		if page > 100000 {
			return nil, errors.New("objstore: s3 list: too many pages")
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// Open opens one object.
func (s S3) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	resp, err := s.do(ctx, s.objectURL(key, nil), "")
	if err != nil {
		return nil, fmt.Errorf("objstore: s3 get %q: %w", key, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, fmt.Errorf("objstore: s3 get %q: %w", key, statusError(resp))
	}
	return resp.Body, nil
}

// Head returns the first n bytes of an object, by a ranged GET (a store that ignores the range answers 200 with
// the whole object, of which n bytes are read).
func (s S3) Head(ctx context.Context, key string, n int64) ([]byte, error) {
	if n <= 0 {
		return nil, nil
	}
	resp, err := s.do(ctx, s.objectURL(key, nil), fmt.Sprintf("bytes=0-%d", n-1))
	if err != nil {
		return nil, fmt.Errorf("objstore: s3 head %q: %w", key, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent:
		return io.ReadAll(io.LimitReader(resp.Body, n))
	case http.StatusRequestedRangeNotSatisfiable:
		return nil, nil // an empty object
	}
	return nil, fmt.Errorf("objstore: s3 head %q: %w", key, statusError(resp))
}

// get is a GET whose whole body is wanted.
func (s S3) get(ctx context.Context, u, rng string) ([]byte, int, error) {
	resp, err := s.do(ctx, u, rng)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return nil, resp.StatusCode, statusError(resp)
	}
	b, err := io.ReadAll(resp.Body)
	return b, resp.StatusCode, err
}

// do sends a signed GET, retrying a 5xx or a connection failure.
func (s S3) do(ctx context.Context, u, rng string) (*http.Response, error) {
	retries := s.Retries
	if retries <= 0 {
		retries = 3
	}
	var last error
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			pause := time.Duration(100<<(2*(attempt-1))) * time.Millisecond // 100 ms, 400 ms, 1.6 s
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(pause):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		s.sign(req)
		resp, err := s.client().Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			last = err
			continue
		}
		if resp.StatusCode >= 500 {
			last = statusError(resp)
			resp.Body.Close()
			continue
		}
		return resp, nil
	}
	return nil, fmt.Errorf("after %d retries: %w", retries, last)
}

// statusError reads an S3 error answer (<Error><Code>...</Code>...) into an error; a 404 wraps ErrNotFound.
func statusError(resp *http.Response) error {
	var e struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	_ = xml.Unmarshal(b, &e)
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w (%d %s)", ErrNotFound, resp.StatusCode, e.Code)
	}
	if e.Code != "" {
		return fmt.Errorf("%d %s: %s", resp.StatusCode, e.Code, e.Message)
	}
	return fmt.Errorf("status %d", resp.StatusCode)
}

const (
	emptyPayloadSHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	amzDateLayout      = "20060102T150405Z"
)

// sign adds the SigV4 headers and the Authorization header of a request with an empty body: x-amz-date,
// x-amz-content-sha256 (the empty payload's hash), x-amz-security-token with a session token, and the signature
// over host, range (when sent) and those.
func (s S3) sign(req *http.Request) {
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	t := now().UTC()
	amzDate := t.Format(amzDateLayout)
	date := t.Format("20060102")
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", emptyPayloadSHA256)
	if s.Creds.SessionToken != "" {
		req.Header.Set("x-amz-security-token", s.Creds.SessionToken)
	}
	names := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	if req.Header.Get("Range") != "" {
		names = append(names, "range")
	}
	if s.Creds.SessionToken != "" {
		names = append(names, "x-amz-security-token")
	}
	sort.Strings(names)
	var canon strings.Builder
	for _, n := range names {
		v := req.Host
		if n != "host" {
			v = strings.Join(strings.Fields(req.Header.Get(n)), " ")
		}
		if req.Host == "" && n == "host" {
			v = req.URL.Host
		}
		canon.WriteString(n + ":" + v + "\n")
	}
	signed := strings.Join(names, ";")
	canonicalRequest := strings.Join([]string{http.MethodGet, req.URL.EscapedPath(), canonicalQuery(req.URL.Query()),
		canon.String(), signed, emptyPayloadSHA256}, "\n")
	scope := date + "/" + s.Region + "/s3/aws4_request"
	toSign := strings.Join([]string{"AWS4-HMAC-SHA256", amzDate, scope, hexSHA256([]byte(canonicalRequest))}, "\n")
	key := hmacSHA256([]byte("AWS4"+s.Creds.SecretKey), date)
	key = hmacSHA256(key, s.Region)
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(key, toSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+s.Creds.AccessKey+"/"+scope+
		", SignedHeaders="+signed+", Signature="+sig)
}

// SignatureOf recomputes a request's SigV4 signature for a secret, from the request as received: the method, the
// path as sent, the query, the headers the Authorization header lists as signed, and the x-amz-content-sha256
// header. A server that verifies signatures (a test fake) compares it with the Authorization header's Signature.
func SignatureOf(req *http.Request, secret string) (string, error) {
	auth := req.Header.Get("Authorization")
	const pfx = "AWS4-HMAC-SHA256 "
	if !strings.HasPrefix(auth, pfx) {
		return "", errors.New("not a SigV4 Authorization header")
	}
	var cred, signed string
	for _, part := range strings.Split(strings.TrimPrefix(auth, pfx), ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "Credential":
			cred = v
		case "SignedHeaders":
			signed = v
		}
	}
	scopeParts := strings.SplitN(cred, "/", 2)
	if len(scopeParts) != 2 || signed == "" {
		return "", errors.New("malformed Authorization header")
	}
	scope := scopeParts[1] // date/region/service/aws4_request
	sp := strings.Split(scope, "/")
	if len(sp) != 4 {
		return "", errors.New("malformed credential scope")
	}
	var canon strings.Builder
	for _, n := range strings.Split(signed, ";") {
		v := req.Host
		if n != "host" {
			v = strings.Join(strings.Fields(req.Header.Get(n)), " ")
		}
		canon.WriteString(n + ":" + v + "\n")
	}
	path := req.URL.EscapedPath()
	canonicalRequest := strings.Join([]string{req.Method, path, canonicalQuery(req.URL.Query()), canon.String(), signed,
		req.Header.Get("x-amz-content-sha256")}, "\n")
	toSign := strings.Join([]string{"AWS4-HMAC-SHA256", req.Header.Get("x-amz-date"), scope, hexSHA256([]byte(canonicalRequest))}, "\n")
	key := hmacSHA256([]byte("AWS4"+secret), sp[0])
	key = hmacSHA256(key, sp[1])
	key = hmacSHA256(key, sp[2])
	key = hmacSHA256(key, sp[3])
	return hex.EncodeToString(hmacSHA256(key, toSign)), nil
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func hexSHA256(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// awsEncode is URI encoding as SigV4 defines it: unreserved characters as they are, every other byte as %XX
// with upper-case hex digits; "/" is kept when keepSlash.
func awsEncode(s string, keepSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && keepSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func encodePath(key string) string { return awsEncode(key, true) }

// canonicalQuery is the query string SigV4 signs: parameters sorted by encoded name, each name and value encoded.
func canonicalQuery(q url.Values) string {
	type kv struct{ k, v string }
	var pairs []kv
	for k, vs := range q {
		for _, v := range vs {
			pairs = append(pairs, kv{awsEncode(k, false), awsEncode(v, false)})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, p.k+"="+p.v)
	}
	return strings.Join(parts, "&")
}

// LoopbackOrHTTPS reports whether an endpoint URL is safe to send credentials to: https, or http to a loopback
// address (a local fake).
func LoopbackOrHTTPS(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.Path != "" && u.Path != "/" || u.RawQuery != "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		host := u.Hostname()
		if host == "localhost" {
			return true
		}
		ip := net.ParseIP(host)
		return ip != nil && ip.IsLoopback()
	}
	return false
}

// Sign signs a request as the store would send it: for tests of the signature.
func Sign(s S3, req *http.Request) { s.sign(req) }
