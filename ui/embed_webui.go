//go:build webui

package ui

import "embed"

const (
	root     = ".output/public"
	embedded = true
)

// The all: prefix includes the _nuxt directory, which go:embed would
// otherwise skip because its name starts with an underscore.
//
//go:embed all:.output/public
var files embed.FS
