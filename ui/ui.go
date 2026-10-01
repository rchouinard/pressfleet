// Package ui embeds the dashboard's static files (design doc §3) so the
// control plane binary serves it without a Node.js runtime.
//
// It is a Go package only because go:embed can't reach outside a package
// directory. Which files are embedded depends on the build tag:
//
//   - default: placeholder/, a one-page notice, so `go build`, `go test`, and
//     the linters work on a machine without Node.js and the repository needs
//     no build output committed.
//   - -tags webui: .output/public, the output of `nuxt generate`. `make ui`
//     produces it and `make build` passes the tag when it exists. The build
//     fails (no matching files) if the tag is set but the UI wasn't built, so
//     a release can't silently ship the placeholder.
package ui

import "io/fs"

// Files returns the embedded dashboard, rooted at its index.html.
func Files() fs.FS {
	sub, err := fs.Sub(files, root)
	if err != nil {
		// fs.Sub only fails for an invalid path, which is a programming error.
		panic(err)
	}
	return sub
}

// Embedded reports whether the real dashboard is embedded, as opposed to the
// placeholder page.
func Embedded() bool { return embedded }
