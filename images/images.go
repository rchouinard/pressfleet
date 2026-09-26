// Package images embeds the site image version manifest (manifest.json,
// design doc §5.5) so the control plane knows which site images exist. It
// exists as a Go package only because go:embed cannot reach files outside the
// embedding package's directory; package catalog parses the manifest.
package images

import (
	"bytes"
	_ "embed" // go:embed of a single file into a []byte
)

//go:embed manifest.json
var manifest []byte

// Manifest returns a copy of the embedded manifest.json.
func Manifest() []byte {
	return bytes.Clone(manifest)
}
