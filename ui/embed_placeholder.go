//go:build !webui

package ui

import "embed"

const (
	root     = "placeholder"
	embedded = false
)

//go:embed placeholder
var files embed.FS
