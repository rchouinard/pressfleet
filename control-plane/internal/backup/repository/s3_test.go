package repository

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The signing examples from the Amazon S3 documentation ("Examples:
// Signature Calculations in AWS Signature Version 4", header-based
// authentication).
func TestSignAWSExamples(t *testing.T) {
	c := &s3Client{
		endpoint: &url.URL{Scheme: "https", Host: "s3.amazonaws.com"},
		bucket:   "examplebucket",
		region:   "us-east-1",
		keyID:    "AKIAIOSFODNN7EXAMPLE",
		secret:   "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		now:      func() time.Time { return time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC) },
	}
	tests := []struct {
		name    string
		key     string
		query   url.Values
		headers map[string]string
		signed  string
		want    string
	}{
		{
			name: "GET object", key: "test.txt", headers: map[string]string{"Range": "bytes=0-9"},
			signed: "host;range;x-amz-content-sha256;x-amz-date",
			want:   "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41",
		},
		{
			name: "GET bucket lifecycle", query: url.Values{"lifecycle": {""}},
			signed: "host;x-amz-content-sha256;x-amz-date",
			want:   "fea454ca298b7da1c68078a5d1bdbfbbe0d65c699e0f91ac7a200a0136783543",
		},
		{
			name: "list objects", query: url.Values{"max-keys": {"2"}, "prefix": {"J"}},
			signed: "host;x-amz-content-sha256;x-amz-date",
			want:   "34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := c.objectURL(tt.key, tt.query)
			if u.Host != "examplebucket.s3.amazonaws.com" {
				t.Errorf("host = %q, want virtual-hosted-style", u.Host)
			}
			req, err := http.NewRequest(http.MethodGet, u.String(), nil)
			if err != nil {
				t.Fatal(err)
			}
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			c.sign(req, emptySHA256)
			want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
				"SignedHeaders=" + tt.signed + ", Signature=" + tt.want
			if got := req.Header.Get("Authorization"); got != want {
				t.Errorf("Authorization =\n%s\nwant\n%s", got, want)
			}
		})
	}
}

func TestObjectURLPathStyle(t *testing.T) {
	c := &s3Client{endpoint: &url.URL{Scheme: "http", Host: "minio:9000"}, bucket: "pf-backups"}
	got := c.objectURL("pressfleet/01j/data/ab/c d", url.Values{"prefix": {"a/b"}}).String()
	want := "http://minio:9000/pf-backups/pressfleet/01j/data/ab/c%20d?prefix=a%2Fb"
	if got != want {
		t.Errorf("objectURL = %s, want %s", got, want)
	}
	// Dotted bucket names can't be virtual-hosted on Amazon S3 with TLS.
	aws := &s3Client{endpoint: &url.URL{Scheme: "https", Host: "s3.eu-west-1.amazonaws.com"}, bucket: "a.b"}
	if got := aws.objectURL("k", nil).String(); got != "https://s3.eu-west-1.amazonaws.com/a.b/k" {
		t.Errorf("objectURL = %s", got)
	}
}

func TestS3Location(t *testing.T) {
	tests := []struct {
		cfg  S3Config
		want string
	}{
		{S3Config{Endpoint: "https://s3.example.com/", Bucket: "b"}, "s3:https://s3.example.com/b/01group"},
		{S3Config{Endpoint: "http://minio:9000", Bucket: "b", Prefix: "pf/prod"}, "s3:http://minio:9000/b/pf/prod/01group"},
	}
	for _, tt := range tests {
		loc := s3Location(tt.cfg, "01group")
		if loc != tt.want {
			t.Errorf("s3Location(%+v) = %s, want %s", tt.cfg, loc, tt.want)
		}
		obj, err := parseS3Location(loc)
		if err != nil {
			t.Fatal(err)
		}
		wantKey := strings.TrimPrefix(tt.cfg.Prefix+"/01group", "/")
		if obj.bucket != "b" || obj.key != wantKey || obj.endpoint.String()+"/" != strings.TrimSuffix(tt.cfg.Endpoint, "/")+"/" {
			t.Errorf("parseS3Location(%s) = %+v", loc, obj)
		}
	}
	for _, bad := range []string{"/var/lib/x", "s3:minio/b/k", "s3:https://h/b", "s3:https://h/"} {
		if _, err := parseS3Location(bad); err == nil {
			t.Errorf("parseS3Location(%q) accepted", bad)
		}
	}
}
