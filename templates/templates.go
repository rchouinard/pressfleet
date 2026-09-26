// Package templates embeds the Compose templates that PressFleet renders
// (design doc §4.1, §4.7). It exists as a Go package only because go:embed
// cannot reach files outside the embedding package's directory.
package templates

import (
	"embed"
	"io/fs"
)

// files holds every embedded template tree. Per-site templates
// (templates/compose, #24) join the embed pattern once they exist.
//
//go:embed platform
var files embed.FS

// Platform returns the platform services definition: the Compose templates
// and the pinned image list (images.yaml).
func Platform() fs.FS {
	return mustSub("platform")
}

func mustSub(dir string) fs.FS {
	sub, err := fs.Sub(files, dir)
	if err != nil {
		// fs.Sub only fails for an invalid path, which is a programming error.
		panic(err)
	}
	return sub
}
