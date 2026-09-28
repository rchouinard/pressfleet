package repository

import (
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
	"slices"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// s3Client is the small part of the S3 API that restic lacks: listing,
// downloading, and deleting a repository's objects, for export and delete.
// restic itself does everything else. It signs requests with AWS Signature
// Version 4 and uses path-style URLs (<endpoint>/<bucket>/<key>), which
// MinIO and other S3-compatible services expect, except on Amazon S3,
// which gets virtual-hosted-style URLs like restic uses.
//
// A dependency on an S3 SDK would be larger than this file.
type s3Client struct {
	endpoint *url.URL
	bucket   string
	region   string
	keyID    redact.Secret
	secret   redact.Secret
	http     *http.Client
	now      func() time.Time
}

// defaultS3Region is used for signing when no region is configured; MinIO
// accepts it by default.
const defaultS3Region = "us-east-1"

// errS3Status is wrapped by errors from S3 responses with an unexpected
// status.
var errS3Status = errors.New("unexpected S3 response")

// objectURL returns the URL of key (which may be empty for the bucket).
func (c *s3Client) objectURL(key string, query url.Values) *url.URL {
	u := *c.endpoint
	escKey := escapePath(key)
	if virtualHosted(c.endpoint.Hostname(), c.bucket) {
		u.Host = c.bucket + "." + u.Host
		u.Path = "/" + key
		u.RawPath = "/" + escKey
	} else {
		u.Path = "/" + c.bucket + "/" + key
		u.RawPath = "/" + c.bucket + "/" + escKey
	}
	u.RawQuery = canonicalQuery(query)
	return &u
}

// virtualHosted reports whether the bucket is addressed as a subdomain:
// on Amazon S3, which is phasing out path-style URLs, for bucket names that
// are valid in a host name.
func virtualHosted(host, bucket string) bool {
	return (host == "amazonaws.com" || strings.HasSuffix(host, ".amazonaws.com")) && !strings.Contains(bucket, ".")
}

// do sends a signed request and returns the response if its status is
// wantStatus; otherwise the body is drained into the error.
func (c *s3Client) do(ctx context.Context, method string, u *url.URL, wantStatus ...int) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return nil, err
	}
	c.sign(req, emptySHA256)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(wantStatus, resp.StatusCode) {
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("%w: %s %s: %s: %s", errS3Status, method, u.Redacted(), resp.Status, s3ErrorMessage(body))
	}
	return resp, nil
}

// s3ErrorMessage extracts the code and message of an S3 error document.
func s3ErrorMessage(body []byte) string {
	var e struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	if xml.Unmarshal(body, &e) == nil && e.Code != "" {
		return e.Code + ": " + e.Message
	}
	return strings.TrimSpace(string(body))
}

// list returns every key under prefix. A missing bucket has no keys.
func (c *s3Client) list(ctx context.Context, prefix string) ([]string, error) {
	var (
		keys  []string
		token string
	)
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := c.do(ctx, http.MethodGet, c.objectURL("", q), http.StatusOK, http.StatusNotFound)
		if err != nil {
			return nil, fmt.Errorf("listing s3 objects: %w", err)
		}
		if resp.StatusCode == http.StatusNotFound {
			_ = resp.Body.Close()
			return nil, nil
		}
		var page struct {
			Contents []struct {
				Key string `xml:"Key"`
			} `xml:"Contents"`
			IsTruncated           bool   `xml:"IsTruncated"`
			NextContinuationToken string `xml:"NextContinuationToken"`
		}
		err = xml.NewDecoder(resp.Body).Decode(&page)
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("listing s3 objects: %w", err)
		}
		for _, o := range page.Contents {
			keys = append(keys, o.Key)
		}
		if !page.IsTruncated {
			return keys, nil
		}
		if page.NextContinuationToken == "" || page.NextContinuationToken == token {
			return nil, errors.New("listing s3 objects: truncated listing without a continuation token")
		}
		token = page.NextContinuationToken
	}
}

// exists reports whether an object exists. A missing bucket has no
// objects.
func (c *s3Client) exists(ctx context.Context, key string) (bool, error) {
	resp, err := c.do(ctx, http.MethodHead, c.objectURL(key, nil), http.StatusOK, http.StatusNotFound)
	if err != nil {
		return false, fmt.Errorf("checking s3 object: %w", err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK, nil
}

// get writes the object's content to w.
func (c *s3Client) get(ctx context.Context, key string, w io.Writer) error {
	resp, err := c.do(ctx, http.MethodGet, c.objectURL(key, nil), http.StatusOK)
	if err != nil {
		return fmt.Errorf("downloading s3 object: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.Copy(w, resp.Body); err != nil {
		return fmt.Errorf("downloading s3 object %s: %w", key, err)
	}
	return nil
}

// remove deletes an object; deleting a missing object succeeds.
func (c *s3Client) remove(ctx context.Context, key string) error {
	resp, err := c.do(ctx, http.MethodDelete, c.objectURL(key, nil), http.StatusNoContent, http.StatusOK, http.StatusNotFound)
	if err != nil {
		return fmt.Errorf("deleting s3 object: %w", err)
	}
	_ = resp.Body.Close()
	return nil
}

// emptySHA256 is the hex SHA-256 of an empty body.
const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// sign adds AWS Signature Version 4 headers to req. Every header already set
// on req is signed, along with host, x-amz-date, and x-amz-content-sha256.
func (c *s3Client) sign(req *http.Request, payloadHash string) {
	t := c.now().UTC()
	amzDate := t.Format("20060102T150405Z")
	day := t.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	headers := map[string]string{"host": req.URL.Host}
	for k, v := range req.Header {
		headers[strings.ToLower(k)] = strings.Join(v, ",")
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	slices.Sort(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + strings.TrimSpace(headers[k]) + "\n")
	}
	signed := strings.Join(names, ";")

	path := req.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	canonical := strings.Join([]string{
		req.Method, path, canonicalQuery(req.URL.Query()), canonHeaders.String(), signed, payloadHash,
	}, "\n")
	scope := day + "/" + c.region + "/s3/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hexSHA256(canonical)

	key := hmacSHA256([]byte("AWS4"+c.secret.Reveal()), day)
	key = hmacSHA256(key, c.region)
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(key, toSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+c.keyID.Reveal()+"/"+scope+
		", SignedHeaders="+signed+", Signature="+sig)
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func hexSHA256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// canonicalQuery encodes query parameters as SigV4 requires: sorted, with
// every byte but the unreserved characters percent-encoded.
func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var parts []string
	for _, k := range keys {
		vals := slices.Clone(q[k])
		slices.Sort(vals)
		for _, v := range vals {
			parts = append(parts, uriEncode(k, true)+"="+uriEncode(v, true))
		}
	}
	return strings.Join(parts, "&")
}

// escapePath percent-encodes an object key for the URL path, keeping
// slashes.
func escapePath(key string) string { return uriEncode(key, false) }

// uriEncode percent-encodes s as SigV4 specifies; encodeSlash controls
// whether '/' is encoded.
func uriEncode(s string, encodeSlash bool) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := range len(s) {
		ch := s[i]
		switch {
		case 'A' <= ch && ch <= 'Z', 'a' <= ch && ch <= 'z', '0' <= ch && ch <= '9',
			ch == '-', ch == '_', ch == '.', ch == '~', ch == '/' && !encodeSlash:
			b.WriteByte(ch)
		default:
			b.WriteByte('%')
			b.WriteByte(hexDigits[ch>>4])
			b.WriteByte(hexDigits[ch&15])
		}
	}
	return b.String()
}
