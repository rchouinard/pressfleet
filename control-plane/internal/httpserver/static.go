package httpserver

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ErrNoIndex means the UI file system has no index.html.
var ErrNoIndex = errors.New("the UI has no index.html")

// minGzipSize is the size below which compressing a file isn't worth the
// CPU on the client and the extra header bytes.
const minGzipSize = 1024

// Cache policies. Files under /_nuxt/ carry a content hash in their names, so
// they never change; everything else must be revalidated so a new release
// takes effect at once.
const (
	cacheImmutable  = "public, max-age=31536000, immutable"
	cacheRevalidate = "no-cache"
)

// nuxtAssetsPrefix is where the build puts hashed assets.
const nuxtAssetsPrefix = "_nuxt/"

// nuxtLatestBuild is polled by the app to detect new releases, so unlike the
// rest of _nuxt/ its name never changes.
const nuxtLatestBuild = "_nuxt/builds/latest.json"

// UI serves the dashboard, a single-page app, from a file system.
type UI struct {
	files map[string]*staticFile
	index *staticFile
	csp   string
}

type staticFile struct {
	body  []byte
	gzip  []byte // nil if the file isn't compressible or too small
	etag  string
	ctype string
	cache string
}

// NewUI loads every file in fsys into memory (a dashboard is a few hundred
// kilobytes) so requests never touch the file system, and precomputes ETags,
// gzip bodies, and the Content-Security-Policy. fsys must contain index.html.
func NewUI(fsys fs.FS) (*UI, error) {
	u := &UI{files: make(map[string]*staticFile)}
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		u.files[name] = newStaticFile(name, body)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("loading the UI: %w", err)
	}
	if u.index = u.files["index.html"]; u.index == nil {
		return nil, ErrNoIndex
	}
	u.csp = contentSecurityPolicy(u.index.body)
	return u, nil
}

func newStaticFile(name string, body []byte) *staticFile {
	sum := sha256.Sum256(body)
	f := &staticFile{
		body:  body,
		etag:  `"` + base64.RawURLEncoding.EncodeToString(sum[:12]) + `"`,
		ctype: contentType(name),
		cache: cacheRevalidate,
	}
	if strings.HasPrefix(name, nuxtAssetsPrefix) && name != nuxtLatestBuild {
		f.cache = cacheImmutable
	}
	if len(body) >= minGzipSize && compressible(f.ctype) {
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		_, _ = zw.Write(body)
		_ = zw.Close()
		if buf.Len() < len(body) {
			f.gzip = buf.Bytes()
		}
	}
	return f
}

var inlineScript = regexp.MustCompile(`(?is)<script(\s[^>]*)?>(.*?)</script>`)

// contentSecurityPolicy returns a policy for the SPA. The page's own scripts
// are external, but Nuxt writes a few small inline ones into index.html (the
// import map, the color-mode bootstrap, the runtime config). Rather than
// allowing 'unsafe-inline', the policy lists the hash of each, so only the
// scripts that shipped in this build run.
//
// form-action allows http: and https: because the one-click WordPress login
// POSTs a token form to the site's own domain, which is any host the operator
// has attached. The token is single-use and short-lived, and the form is
// built by the dashboard, not by page content.
//
// Styles allow 'unsafe-inline' because the component library positions
// popovers and sets CSS variables through style attributes and elements.
func contentSecurityPolicy(index []byte) string {
	scripts := []string{"'self'"}
	for _, m := range inlineScript.FindAllSubmatch(index, -1) {
		if bytes.Contains(m[1], []byte("src=")) || len(bytes.TrimSpace(m[2])) == 0 {
			continue
		}
		if t := scriptType(m[1]); t != "" && t != "module" && t != "importmap" && !strings.Contains(t, "javascript") {
			continue // data blocks such as application/json aren't executed
		}
		sum := sha256.Sum256(m[2])
		scripts = append(scripts, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
	}
	return strings.Join([]string{
		"default-src 'self'",
		"script-src " + strings.Join(scripts, " "),
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data:",
		"object-src 'none'",
		"base-uri 'self'",
		"form-action 'self' https: http:",
		"frame-ancestors 'none'",
	}, "; ")
}

var typeAttr = regexp.MustCompile(`(?i)\btype\s*=\s*["']?([^"'\s>]+)`)

func scriptType(attrs []byte) string {
	if m := typeAttr.FindSubmatch(attrs); m != nil {
		return strings.ToLower(string(m[1]))
	}
	return ""
}

// Handler serves the UI: a file if one matches the path, otherwise index.html
// so the app's client-side router can handle the URL. Paths that name a file
// (they have an extension, or are under /_nuxt/) but don't exist are real 404s
// rather than index.html, so a stale asset reference fails loudly instead of
// loading HTML as JavaScript. It answers GET and HEAD only.
func (u *UI) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", u.csp)
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")

		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			h.Set("Allow", "GET, HEAD")
			WriteProblem(w, r, NewProblem(http.StatusMethodNotAllowed, "Method "+r.Method+" is not allowed for this resource."))
			return
		}

		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		f := u.files[name]
		switch {
		case name == "":
			f = u.index
		case f != nil:
		case strings.HasPrefix(name, nuxtAssetsPrefix) || path.Ext(name) != "":
			WriteProblem(w, r, NewProblem(http.StatusNotFound, "No resource exists at this path."))
			return
		default:
			f = u.index
		}
		f.serve(w, r)
	})
}

func (f *staticFile) serve(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", f.ctype)
	h.Set("Cache-Control", f.cache)
	body, etag := f.body, f.etag
	if f.gzip != nil {
		h.Add("Vary", "Accept-Encoding")
		if acceptsGzip(r) {
			h.Set("Content-Encoding", "gzip")
			body, etag = f.gzip, strings.TrimSuffix(f.etag, `"`)+`-gzip"`
		}
	}
	h.Set("ETag", etag)
	// A zero modtime leaves out Last-Modified; the ETag does revalidation.
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
}

func acceptsGzip(r *http.Request) bool {
	for _, v := range r.Header.Values("Accept-Encoding") {
		for _, enc := range strings.Split(v, ",") {
			name, params, _ := strings.Cut(strings.TrimSpace(enc), ";")
			if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
				continue
			}
			// "gzip;q=0" means "not acceptable".
			if q, ok := strings.CutPrefix(strings.ReplaceAll(params, " ", ""), "q="); ok {
				if v, err := strconv.ParseFloat(q, 64); err == nil && v == 0 {
					continue
				}
			}
			return true
		}
	}
	return false
}

// WithUI serves the dashboard for every request the API doesn't own. The API
// handles /api/..., /healthz, and /readyz, and every method other than GET
// and HEAD, so an unknown API path is a Problem Details 404 and never
// index.html.
func WithUI(api http.Handler, ui *UI) http.Handler {
	static := ui.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ownedByAPI(r) {
			api.ServeHTTP(w, r)
			return
		}
		static.ServeHTTP(w, r)
	})
}

func ownedByAPI(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return true
	}
	p := r.URL.Path
	return p == "/api" || strings.HasPrefix(p, "/api/") || p == "/healthz" || p == "/readyz"
}

// contentType maps a file name to its media type. It doesn't use package
// mime, which reads the host's mime.types and so varies between machines.
func contentType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".json", ".map":
		return "application/json"
	case ".webmanifest":
		return "application/manifest+json"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".avif":
		return "image/avif"
	case ".ico":
		return "image/x-icon"
	case ".woff":
		return "font/woff"
	case ".woff2":
		return "font/woff2"
	case ".txt":
		return "text/plain; charset=utf-8"
	case ".wasm":
		return "application/wasm"
	default:
		return "application/octet-stream"
	}
}

func compressible(ctype string) bool {
	return strings.HasPrefix(ctype, "text/") || strings.Contains(ctype, "json") || strings.Contains(ctype, "svg") ||
		strings.Contains(ctype, "javascript") || ctype == "image/x-icon"
}
