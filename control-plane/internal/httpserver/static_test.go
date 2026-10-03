package httpserver_test

import (
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/rchouinard/pressfleet/control-plane/internal/httpserver"
	"github.com/rchouinard/pressfleet/ui"
)

const testIndex = `<!DOCTYPE html><html><head>` +
	`<script type="importmap">{"imports":{"#entry":"/_nuxt/entry.js"}}</script>` +
	`<script>window.__NUXT__={}</script>` +
	`<script type="module" src="/_nuxt/entry.js"></script>` +
	`</head><body><div id="__nuxt"></div>` +
	`<script type="application/json" id="__NUXT_DATA__">[1]</script></body></html>`

var bigJS = "console.log('" + strings.Repeat("pressfleet ", 500) + "');"

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":                 {Data: []byte(testIndex)},
		"200.html":                   {Data: []byte("<html>200</html>")},
		"favicon.ico":                {Data: []byte("icon")},
		"_nuxt/entry.js":             {Data: []byte(bigJS)},
		"_nuxt/entry.css":            {Data: []byte("body{}")},
		"_nuxt/builds/latest.json":   {Data: []byte(`{"id":"abc"}`)},
		"_nuxt/builds/meta/abc.json": {Data: []byte(`{"id":"abc"}`)},
		"assets/logo.svg":            {Data: []byte("<svg/>")},
		"docs/index.html":            {Data: []byte("nested index")},
	}
}

// apiStub stands in for the API: it answers for the paths it owns and tells
// the tests when it was reached.
func apiStub() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-From", "api")
		w.WriteHeader(http.StatusTeapot)
	})
}

func newUIHandler(t *testing.T, fsys fstest.MapFS) http.Handler {
	t.Helper()
	ui, err := httpserver.NewUI(fsys)
	if err != nil {
		t.Fatalf("NewUI: %v", err)
	}
	return httpserver.WithUI(apiStub(), ui)
}

func do(h http.Handler, method, target string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestUIRouting(t *testing.T) {
	h := newUIHandler(t, testFS())

	tests := []struct {
		name, method, path string
		status             int
		body               string // substring; "" skips the check
		api                bool
		ctype              string
	}{
		{name: "root", method: "GET", path: "/", status: 200, body: `id="__nuxt"`, ctype: "text/html; charset=utf-8"},
		{name: "index.html", method: "GET", path: "/index.html", status: 200, body: `id="__nuxt"`},
		{name: "spa route", method: "GET", path: "/sites/42/backups", status: 200, body: `id="__nuxt"`},
		{name: "spa route trailing slash", method: "GET", path: "/sites/", status: 200, body: `id="__nuxt"`},
		{name: "directory with its own index is a route", method: "GET", path: "/docs", status: 200, body: `id="__nuxt"`},
		{name: "asset", method: "GET", path: "/_nuxt/entry.css", status: 200, body: "body{}", ctype: "text/css; charset=utf-8"},
		{name: "script", method: "GET", path: "/_nuxt/entry.js", status: 200, ctype: "text/javascript; charset=utf-8"},
		{name: "svg", method: "GET", path: "/assets/logo.svg", status: 200, ctype: "image/svg+xml"},
		{name: "missing hashed asset", method: "GET", path: "/_nuxt/gone.js", status: 404},
		{name: "missing nuxt path without extension", method: "GET", path: "/_nuxt/gone", status: 404},
		{name: "missing file elsewhere", method: "GET", path: "/robots.txt", status: 404},
		{name: "traversal stays inside", method: "GET", path: "/_nuxt/../../etc/passwd", status: 200, body: `id="__nuxt"`},
		{name: "api", method: "GET", path: "/api/v1/sites", status: 418, api: true},
		{name: "unknown api path is not index.html", method: "GET", path: "/api/v1/nope", status: 418, api: true},
		{name: "api root", method: "GET", path: "/api", status: 418, api: true},
		{name: "api prefix lookalike is a route", method: "GET", path: "/apiary", status: 200, body: `id="__nuxt"`},
		{name: "healthz", method: "GET", path: "/healthz", status: 418, api: true},
		{name: "readyz", method: "GET", path: "/readyz", status: 418, api: true},
		{name: "post goes to the API", method: "POST", path: "/anything", status: 418, api: true},
		{name: "delete goes to the API", method: "DELETE", path: "/sites/1", status: 418, api: true},
		{name: "head root", method: "HEAD", path: "/", status: 200, ctype: "text/html; charset=utf-8"},
		{name: "head spa route", method: "HEAD", path: "/sites", status: 200},
		{name: "head missing asset", method: "HEAD", path: "/_nuxt/gone.js", status: 404},
		{name: "head api", method: "HEAD", path: "/api/v1/sites", status: 418, api: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(h, tt.method, tt.path)
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tt.status, rec.Body.String())
			}
			if got := rec.Header().Get("X-From") == "api"; got != tt.api {
				t.Errorf("served by API = %v, want %v", got, tt.api)
			}
			if tt.body != "" && !strings.Contains(rec.Body.String(), tt.body) {
				t.Errorf("body = %q, want it to contain %q", rec.Body.String(), tt.body)
			}
			if tt.method == http.MethodHead && tt.status == 200 && rec.Body.Len() != 0 {
				t.Errorf("HEAD returned a %d-byte body", rec.Body.Len())
			}
			if tt.ctype != "" && rec.Header().Get("Content-Type") != tt.ctype {
				t.Errorf("Content-Type = %q, want %q", rec.Header().Get("Content-Type"), tt.ctype)
			}
		})
	}
}

func TestUIMissingAssetIsProblemDetails(t *testing.T) {
	rec := do(newUIHandler(t, testFS()), "GET", "/_nuxt/gone.js")
	if got := rec.Header().Get("Content-Type"); got != httpserver.ContentTypeProblem {
		t.Errorf("Content-Type = %q, want %q", got, httpserver.ContentTypeProblem)
	}
}

func TestUICacheHeaders(t *testing.T) {
	h := newUIHandler(t, testFS())
	tests := []struct{ path, want string }{
		{"/", "no-cache"},
		{"/index.html", "no-cache"},
		{"/some/spa/route", "no-cache"},
		{"/200.html", "no-cache"},
		{"/favicon.ico", "no-cache"},
		{"/_nuxt/entry.js", "public, max-age=31536000, immutable"},
		{"/_nuxt/entry.css", "public, max-age=31536000, immutable"},
		{"/_nuxt/builds/meta/abc.json", "public, max-age=31536000, immutable"},
		// The app polls this to find new releases, so it can't be cached.
		{"/_nuxt/builds/latest.json", "no-cache"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := do(h, "GET", tt.path).Header().Get("Cache-Control"); got != tt.want {
				t.Errorf("Cache-Control = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestUIConditionalRequest(t *testing.T) {
	h := newUIHandler(t, testFS())
	first := do(h, "GET", "/")
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}
	if got := do(h, "GET", "/", "If-None-Match", etag); got.Code != http.StatusNotModified {
		t.Errorf("revalidating: status = %d, want 304", got.Code)
	}
	// An SPA route is the same document and revalidates against the same tag.
	if got := do(h, "GET", "/sites/1", "If-None-Match", etag); got.Code != http.StatusNotModified {
		t.Errorf("revalidating a route: status = %d, want 304", got.Code)
	}
}

func TestUISecurityHeaders(t *testing.T) {
	h := newUIHandler(t, testFS())
	for _, path := range []string{"/", "/sites/1", "/_nuxt/entry.js", "/_nuxt/gone.js"} {
		t.Run(path, func(t *testing.T) {
			hdr := do(h, "GET", path).Header()
			if got := hdr.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q", got)
			}
			if got := hdr.Get("X-Frame-Options"); got != "DENY" {
				t.Errorf("X-Frame-Options = %q", got)
			}
			csp := hdr.Get("Content-Security-Policy")
			for _, want := range []string{"default-src 'self'", "frame-ancestors 'none'", "object-src 'none'", "base-uri 'self'", "form-action 'self' https: http:"} {
				if !strings.Contains(csp, want) {
					t.Errorf("CSP %q lacks %q", csp, want)
				}
			}
			if strings.Contains(csp, "script-src 'self' 'unsafe-inline'") || strings.Contains(csp, "unsafe-eval") {
				t.Errorf("CSP allows inline or eval scripts: %q", csp)
			}
		})
	}
}

func TestUICSPHashesInlineScripts(t *testing.T) {
	csp := do(newUIHandler(t, testFS()), "GET", "/").Header().Get("Content-Security-Policy")
	var scriptSrc string
	for _, d := range strings.Split(csp, "; ") {
		if v, ok := strings.CutPrefix(d, "script-src "); ok {
			scriptSrc = v
		}
	}
	// 'self', the import map, and the runtime config. Not the external
	// script, and not the JSON data block, which never executes.
	if got := strings.Count(scriptSrc, "'sha256-"); got != 2 {
		t.Errorf("script-src has %d hashes, want 2: %q", got, scriptSrc)
	}
	if !strings.HasPrefix(scriptSrc, "'self' ") {
		t.Errorf("script-src = %q, want it to start with 'self'", scriptSrc)
	}
}

func TestUIGzip(t *testing.T) {
	h := newUIHandler(t, testFS())

	rec := do(h, "GET", "/_nuxt/entry.js", "Accept-Encoding", "br, gzip;q=0.8")
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := rec.Header().Get("Vary"); got != "Accept-Encoding" {
		t.Errorf("Vary = %q", got)
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(zr)
	if err != nil || string(body) != bigJS {
		t.Errorf("gzip body differs from the file (err %v)", err)
	}
	gzTag := rec.Header().Get("ETag")

	plain := do(h, "GET", "/_nuxt/entry.js")
	if plain.Header().Get("Content-Encoding") != "" || plain.Body.String() != bigJS {
		t.Error("a client that doesn't accept gzip must get the file as is")
	}
	if plain.Header().Get("ETag") == gzTag {
		t.Error("gzip and identity bodies share an ETag")
	}
	if plain.Header().Get("Vary") != "Accept-Encoding" {
		t.Error("identity response lacks Vary")
	}

	if got := do(h, "GET", "/_nuxt/entry.js", "Accept-Encoding", "gzip;q=0"); got.Header().Get("Content-Encoding") != "" {
		t.Error("gzip;q=0 must not get gzip")
	}
	// Small files aren't worth compressing.
	if got := do(h, "GET", "/_nuxt/entry.css", "Accept-Encoding", "gzip"); got.Header().Get("Content-Encoding") != "" {
		t.Error("a tiny file was compressed")
	}
}

func TestUIMethodNotAllowedDirect(t *testing.T) {
	// WithUI sends non-GET/HEAD to the API, but the handler is safe on its own.
	ui, err := httpserver.NewUI(testFS())
	if err != nil {
		t.Fatal(err)
	}
	rec := do(ui.Handler(), "POST", "/")
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("status = %d, Allow = %q", rec.Code, rec.Header().Get("Allow"))
	}
}

// TestEmbeddedUI checks that whatever the binary embeds (the placeholder, or
// the real UI in a -tags webui build) is something NewUI accepts.
func TestEmbeddedUI(t *testing.T) {
	if _, err := httpserver.NewUI(ui.Files()); err != nil {
		t.Fatalf("NewUI(ui.Files()): %v", err)
	}
}

func TestNewUIRequiresIndex(t *testing.T) {
	_, err := httpserver.NewUI(fstest.MapFS{"_nuxt/entry.js": {Data: []byte("x")}})
	if !errors.Is(err, httpserver.ErrNoIndex) {
		t.Errorf("err = %v, want ErrNoIndex", err)
	}
}
