package httpserver

import "testing"

func TestContentType(t *testing.T) {
	tests := map[string]string{
		"index.html":           "text/html; charset=utf-8",
		"_nuxt/entry.MJS":      "text/javascript; charset=utf-8",
		"app.js":               "text/javascript; charset=utf-8",
		"app.css":              "text/css; charset=utf-8",
		"app.js.map":           "application/json",
		"site.webmanifest":     "application/manifest+json",
		"logo.svg":             "image/svg+xml",
		"a.png":                "image/png",
		"a.jpeg":               "image/jpeg",
		"a.jpg":                "image/jpeg",
		"a.gif":                "image/gif",
		"a.webp":               "image/webp",
		"a.avif":               "image/avif",
		"favicon.ico":          "image/x-icon",
		"font.woff":            "font/woff",
		"font.woff2":           "font/woff2",
		"robots.txt":           "text/plain; charset=utf-8",
		"module.wasm":          "application/wasm",
		"archive.tar.gz":       "application/octet-stream",
		"no-extension":         "application/octet-stream",
		"dir.with.dots/readme": "application/octet-stream",
	}
	for name, want := range tests {
		if got := contentType(name); got != want {
			t.Errorf("contentType(%q) = %q, want %q", name, got, want)
		}
	}
	for ctype, want := range map[string]bool{
		"text/html; charset=utf-8": true, "application/json": true, "image/svg+xml": true,
		"text/javascript; charset=utf-8": true, "image/x-icon": true, "image/png": false, "font/woff2": false,
	} {
		if got := compressible(ctype); got != want {
			t.Errorf("compressible(%q) = %v, want %v", ctype, got, want)
		}
	}
}
