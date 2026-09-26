// Package templates embeds the Compose templates that PressFleet renders
// (design doc §4.1, §4.7). It exists as a Go package only because go:embed
// cannot reach files outside the embedding package's directory.
package templates

import (
	"embed"
	"io/fs"
)

// files holds every embedded template tree.
//
//go:embed platform compose/*.tmpl
var files embed.FS

// Platform returns the platform services definition: the Compose templates
// and the pinned image list (images.yaml).
func Platform() fs.FS {
	return mustSub("platform")
}

// Compose returns the per-site Compose templates (site.yaml.tmpl), rendered
// by the Compose driver into one project per site.
func Compose() fs.FS {
	return mustSub("compose")
}

func mustSub(dir string) fs.FS {
	sub, err := fs.Sub(files, dir)
	if err != nil {
		// fs.Sub only fails for an invalid path, which is a programming error.
		panic(err)
	}
	return sub
}
