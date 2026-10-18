package repository

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeS3 is a path-style S3 server holding one bucket's objects in memory.
// ListObjectsV2 returns at most pageSize keys per page.
type fakeS3 struct {
	t        *testing.T
	bucket   string
	pageSize int

	mu      sync.Mutex
	objects map[string]string
	// noBucket answers every request with NoSuchBucket.
	noBucket bool
	// fail, if set, answers every request with this status and an S3
	// error document.
	fail int
	// lostToken makes truncated listings omit NextContinuationToken.
	lostToken bool
	requests  []string
}

const (
	testKeyID  = "AKIDTEST"
	testSecret = "s3-test-secret-do-not-leak"
)

func newFakeS3(t *testing.T, objects map[string]string) (*fakeS3, *s3Client) {
	t.Helper()
	f := &fakeS3{t: t, bucket: "pf-backups", pageSize: 2, objects: objects}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	c := &s3Client{
		endpoint: u, bucket: f.bucket, region: defaultS3Region,
		keyID: testKeyID, secret: testSecret, http: srv.Client(),
		now: func() time.Time { return time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC) },
	}
	return f, c
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())

	if auth := r.Header.Get("Authorization"); !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential="+testKeyID+"/20261018/us-east-1/s3/aws4_request,") ||
		!strings.Contains(auth, "Signature=") || r.Header.Get("X-Amz-Content-Sha256") != emptySHA256 {
		f.t.Errorf("%s %s: unsigned or badly signed request: %q", r.Method, r.URL, auth)
	}
	if f.fail != 0 {
		w.WriteHeader(f.fail)
		_, _ = w.Write([]byte(`<?xml version="1.0"?><Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>`))
		return
	}
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if bucket != f.bucket || f.noBucket {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<Error><Code>NoSuchBucket</Code><Message>The specified bucket does not exist</Message></Error>`))
		return
	}
	switch {
	case r.Method == http.MethodGet && key == "":
		f.list(w, r.URL.Query())
	case r.Method == http.MethodHead, r.Method == http.MethodGet:
		body, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(body))
		}
	case r.Method == http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *fakeS3) list(w http.ResponseWriter, q url.Values) {
	if q.Get("list-type") != "2" {
		f.t.Errorf("listing without list-type=2: %v", q)
	}
	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, q.Get("prefix")) {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	start := 0
	if tok := q.Get("continuation-token"); tok != "" {
		n, err := strconv.Atoi(tok)
		if err != nil {
			f.t.Errorf("bad continuation token %q", tok)
		}
		start = n
	}
	end := min(start+f.pageSize, len(keys))
	type content struct {
		Key string `xml:"Key"`
	}
	page := struct {
		XMLName               xml.Name  `xml:"ListBucketResult"`
		Contents              []content `xml:"Contents"`
		IsTruncated           bool      `xml:"IsTruncated"`
		NextContinuationToken string    `xml:"NextContinuationToken,omitempty"`
	}{IsTruncated: end < len(keys)}
	for _, k := range keys[start:end] {
		page.Contents = append(page.Contents, content{Key: k})
	}
	if page.IsTruncated && !f.lostToken {
		page.NextContinuationToken = strconv.Itoa(end)
	}
	_ = xml.NewEncoder(w).Encode(page)
}

func TestS3ClientList(t *testing.T) {
	ctx := context.Background()
	objects := map[string]string{
		"repo/config": "c", "repo/keys/k1": "k", "repo/data/00/a": "a", "repo/data/01/b": "b", "repo/index/i": "i",
		"other/config": "x",
	}
	f, c := newFakeS3(t, objects)
	keys, err := c.list(ctx, "repo/")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"repo/config", "repo/data/00/a", "repo/data/01/b", "repo/index/i", "repo/keys/k1"}
	if !slices.Equal(keys, want) {
		t.Errorf("keys = %v, want %v", keys, want)
	}
	if pages := len(f.requests); pages != 3 {
		t.Errorf("%d list requests, want 3 pages of 2: %v", pages, f.requests)
	}
	for _, r := range f.requests {
		if !strings.HasPrefix(r, "GET /pf-backups/?") {
			t.Errorf("request %q isn't a path-style bucket listing", r)
		}
	}

	f.noBucket = true
	if keys, err := c.list(ctx, "repo/"); err != nil || keys != nil {
		t.Errorf("missing bucket: keys %v, err %v; want none", keys, err)
	}

	f.noBucket, f.lostToken = false, true
	if _, err := c.list(ctx, "repo/"); err == nil || !strings.Contains(err.Error(), "without a continuation token") {
		t.Errorf("truncated listing without a token: err %v", err)
	}
}

func TestS3ClientObjects(t *testing.T) {
	ctx := context.Background()
	_, c := newFakeS3(t, map[string]string{"repo/data/a b+c": "content"})

	for key, want := range map[string]bool{"repo/data/a b+c": true, "repo/data/missing": false} {
		if ok, err := c.exists(ctx, key); err != nil || ok != want {
			t.Errorf("exists(%q) = %v, %v; want %v", key, ok, err, want)
		}
	}

	var buf bytes.Buffer
	if err := c.get(ctx, "repo/data/a b+c", &buf); err != nil || buf.String() != "content" {
		t.Errorf("get = %q, %v", buf.String(), err)
	}
	if err := c.get(ctx, "repo/data/missing", &buf); !errors.Is(err, errS3Status) {
		t.Errorf("get missing: err %v, want errS3Status", err)
	}

	dst := filepath.Join(t.TempDir(), "export", "data", "a")
	if err := downloadFile(ctx, c, "repo/data/a b+c", dst); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(dst); err != nil || string(b) != "content" {
		t.Errorf("downloaded %q, %v", b, err)
	}
	if info, err := os.Stat(dst); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("downloaded file: %v, %v; want mode 0600", info, err)
	}

	// Deleting succeeds whether or not the object is there.
	for range 2 {
		if err := c.remove(ctx, "repo/data/a b+c"); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := c.exists(ctx, "repo/data/a b+c"); err != nil || ok {
		t.Errorf("after remove: exists %v, %v", ok, err)
	}
}

func TestS3ClientErrors(t *testing.T) {
	ctx := context.Background()
	f, c := newFakeS3(t, map[string]string{"repo/config": "c"})
	f.fail = http.StatusForbidden
	calls := map[string]func() error{
		"list":   func() error { _, err := c.list(ctx, "repo/"); return err },
		"exists": func() error { _, err := c.exists(ctx, "repo/config"); return err },
		"get":    func() error { return c.get(ctx, "repo/config", &bytes.Buffer{}) },
		"remove": func() error { return c.remove(ctx, "repo/config") },
	}
	for name, call := range calls {
		err := call()
		if !errors.Is(err, errS3Status) {
			t.Errorf("%s: err %v, want errS3Status", name, err)
			continue
		}
		msg := err.Error()
		// HEAD responses have no body, so exists can only report the
		// status.
		if !strings.Contains(msg, "403") || (name != "exists" && !strings.Contains(msg, "AccessDenied: Access Denied")) {
			t.Errorf("%s: error %q lacks the status and the S3 error", name, msg)
		}
		if strings.Contains(msg, testSecret) || strings.Contains(msg, "Signature=") {
			t.Errorf("%s: error %q leaks credentials", name, msg)
		}
	}
}

func TestS3ErrorMessage(t *testing.T) {
	tests := map[string]string{
		`<Error><Code>NoSuchKey</Code><Message>The key does not exist</Message></Error>`: "NoSuchKey: The key does not exist",
		"  plain text from a proxy \n":              "plain text from a proxy",
		`<Error><Message>no code</Message></Error>`: "<Error><Message>no code</Message></Error>",
		"": "",
	}
	for body, want := range tests {
		if got := s3ErrorMessage([]byte(body)); got != want {
			t.Errorf("s3ErrorMessage(%q) = %q, want %q", body, got, want)
		}
	}
}

func TestS3Env(t *testing.T) {
	if env := s3Env(S3Config{}); len(env) != 0 {
		t.Errorf("no region: %v", env)
	}
	if env := s3Env(S3Config{Region: "eu-west-1", SecretAccessKey: "s"}); len(env) != 1 || env["AWS_DEFAULT_REGION"] != "eu-west-1" {
		t.Errorf("region: %v", env)
	}
}
