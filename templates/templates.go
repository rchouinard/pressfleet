// Package templates embeds the Compose and Kubernetes templates that
// PressFleet renders (design doc §4.1, §4.7, §4.8). It exists as a Go package only because go:embed
// cannot reach files outside the embedding package's directory.
package templates

import (
	"embed"
	"io/fs"
)

// files holds every embedded template tree.
//
//go:embed platform compose/*.tmpl k8s/*.tmpl
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

// Kubernetes returns the per-site Kubernetes manifest templates
// (*.yaml.tmpl), rendered by the Kubernetes driver into the site's
// directory and applied to the site's namespace.
func Kubernetes() fs.FS {
	return mustSub("k8s")
}

func mustSub(dir string) fs.FS {
	sub, err := fs.Sub(files, dir)
	if err != nil {
		// fs.Sub only fails for an invalid path, which is a programming error.
		panic(err)
	}
	return sub
}
