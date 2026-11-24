package backup

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// S3Store is a minimal client for S3-compatible storage (AWS S3, Backblaze
// B2, Cloudflare R2, MinIO): PUT (optionally If-None-Match: *), GET (with
// Range), DELETE and ListObjectsV2, signed with AWS Signature Version 4.
// Written here instead of a dependency (D63).
type S3Store struct {
	Endpoint  string // "https://s3.us-east-1.amazonaws.com", "http://127.0.0.1:9000"
	Region    string // "us-east-1"; R2 uses "auto"
	Bucket    string
	Prefix    string // "" or ends with "/"
	AccessKey string
	SecretKey string
	PathStyle bool // bucket in the path (MinIO, most others) instead of the host name
	Client    *http.Client
	// Now is the clock for signatures (tests); nil = time.Now.
	Now func() time.Time
}

func (s *S3Store) Label() string {
	u, err := url.Parse(s.Endpoint)
	host := s.Endpoint
	if err == nil {
		host = u.Host
	}
	return "s3://" + s.Bucket + "/" + strings.TrimSuffix(s.Prefix, "/") + " (" + host + ")"
}

func (s *S3Store) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return &http.Client{Timeout: 10 * time.Minute}
}

// S3Error is a non-success answer from the storage service.
type S3Error struct {
	Status  int
	Code    string
	Message string
}

func (e *S3Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("storage answered %d %s: %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("storage answered %d", e.Status)
}

func (s *S3Store) objectURL(key string, q url.Values) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSuffix(s.Endpoint, "/"))
	if err != nil {
		return nil, fmt.Errorf("bad storage endpoint: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, fmt.Errorf("storage endpoint must start with https:// (or http:// for a local test server)")
	}
	p := ""
	if s.PathStyle {
		p = "/" + s.Bucket
	} else {
		u.Host = s.Bucket + "." + u.Host
	}
	if key != "" {
		p += "/" + key
	}
	if p == "" {
		p = "/"
	}
	u.Path = p
	u.RawPath = uriEncode(p, false)
	u.RawQuery = canonicalQuery(q)
	return u, nil
}

func (s *S3Store) do(ctx context.Context, method, key string, q url.Values, body []byte, hdr http.Header) (*http.Response, error) {
	u, err := s.objectURL(key, q)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.ContentLength = int64(len(body))
	if body == nil {
		req.Body = http.NoBody
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	s.sign(req, body)
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("storage request failed: %w", err)
	}
	return resp, nil
}

func s3Err(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var x struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	_ = xml.Unmarshal(b, &x)
	return &S3Error{Status: resp.StatusCode, Code: x.Code, Message: x.Message}
}

func (s *S3Store) Put(ctx context.Context, key string, data []byte, ifAbsent bool) error {
	h := http.Header{}
	h.Set("Content-Type", "application/octet-stream")
	if ifAbsent {
		h.Set("If-None-Match", "*")
	}
	resp, err := s.do(ctx, http.MethodPut, s.Prefix+key, nil, data, h)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode/100 == 2:
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	case ifAbsent && (resp.StatusCode == http.StatusPreconditionFailed || resp.StatusCode == http.StatusConflict):
		return ErrExists
	}
	return s3Err(resp)
}

func (s *S3Store) get(ctx context.Context, key string, rng string) ([]byte, error) {
	h := http.Header{}
	if rng != "" {
		h.Set("Range", rng)
	}
	resp, err := s.do(ctx, http.MethodGet, s.Prefix+key, nil, nil, h)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, ErrNotFound
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		return []byte{}, nil // empty object
	case resp.StatusCode/100 != 2:
		return nil, s3Err(resp)
	}
	return io.ReadAll(resp.Body)
}

func (s *S3Store) Get(ctx context.Context, key string) ([]byte, error) { return s.get(ctx, key, "") }

func (s *S3Store) GetPrefix(ctx context.Context, key string, n int) ([]byte, error) {
	b, err := s.get(ctx, key, "bytes=0-"+strconv.Itoa(n-1))
	if err == nil && len(b) > n { // a server that ignored Range
		b = b[:n]
	}
	return b, err
}

func (s *S3Store) Delete(ctx context.Context, key string) error {
	resp, err := s.do(ctx, http.MethodDelete, s.Prefix+key, nil, nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 || resp.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return s3Err(resp)
}

func (s *S3Store) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	var out []ObjectInfo
	token := ""
	for {
		q := url.Values{}
		q.Set("list-type", "2")
		q.Set("prefix", s.Prefix+prefix)
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := s.do(ctx, http.MethodGet, "", q, nil, nil)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode/100 != 2 {
			err := s3Err(resp)
			resp.Body.Close()
			return nil, err
		}
		var r struct {
			Contents []struct {
				Key          string `xml:"Key"`
				Size         int64  `xml:"Size"`
				LastModified string `xml:"LastModified"`
			} `xml:"Contents"`
			IsTruncated           bool   `xml:"IsTruncated"`
			NextContinuationToken string `xml:"NextContinuationToken"`
		}
		err = xml.NewDecoder(resp.Body).Decode(&r)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("storage list answer: %w", err)
		}
		for _, c := range r.Contents {
			t, _ := time.Parse(time.RFC3339, c.LastModified)
			out = append(out, ObjectInfo{Key: strings.TrimPrefix(c.Key, s.Prefix), Size: c.Size, Modified: t})
		}
		if !r.IsTruncated || r.NextContinuationToken == "" {
			break
		}
		token = r.NextContinuationToken
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// ---- Signature Version 4 ----

func (s *S3Store) sign(req *http.Request, body []byte) {
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	t := now().UTC()
	amzDate := t.Format("20060102T150405Z")
	date := t.Format("20060102")
	sum := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(sum[:])
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	region := s.Region
	if region == "" {
		region = "us-east-1"
	}
	signed, canonHeaders := canonicalHeaders(req)
	canonical := strings.Join([]string{
		req.Method,
		req.URL.EscapedPath(),
		req.URL.RawQuery,
		canonHeaders,
		signed,
		payloadHash,
	}, "\n")
	scope := date + "/" + region + "/s3/aws4_request"
	h := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(h[:])
	k := hmacSHA256([]byte("AWS4"+s.SecretKey), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(k, toSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+s.AccessKey+"/"+scope+
		", SignedHeaders="+signed+", Signature="+sig)
}

func canonicalHeaders(req *http.Request) (signed, canon string) {
	hs := map[string]string{"host": req.URL.Host}
	for k, v := range req.Header {
		lk := strings.ToLower(k)
		if lk == "authorization" || lk == "user-agent" {
			continue
		}
		hs[lk] = strings.Join(trimAll(v), ",")
	}
	keys := make([]string, 0, len(hs))
	for k := range hs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + ":" + hs[k] + "\n")
	}
	return strings.Join(keys, ";"), b.String()
}

func trimAll(v []string) []string {
	out := make([]string, len(v))
	for i, s := range v {
		out[i] = strings.Join(strings.Fields(s), " ")
	}
	return out
}

func hmacSHA256(key []byte, s string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(s))
	return m.Sum(nil)
}

// uriEncode is SigV4's URI encoding: unreserved characters stay, every
// other byte is %XX (upper case); "/" stays in paths.
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func canonicalQuery(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string{}, q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, uriEncode(k, true)+"="+uriEncode(v, true))
		}
	}
	return strings.Join(parts, "&")
}

// ErrS3Config: the S3 settings are incomplete.
var ErrS3Config = errors.New("cloud storage needs an endpoint, a bucket, an access key and a secret key")
