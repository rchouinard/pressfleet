// Package images embeds the site image version manifest (manifest.json,
// design doc §5.5) so the control plane knows which site images exist, and
// the default plugin list (shared/plugins.json, §5.1 step 7) the site-php
// image bundles. It exists as a Go package only because go:embed cannot
// reach files outside the embedding package's directory; package catalog
// parses the manifest.
package images

import (
	"bytes"
	_ "embed" // go:embed of single files into []byte
)

//go:embed manifest.json
var manifest []byte

//go:embed shared/plugins.json
var plugins []byte

// Manifest returns a copy of the embedded manifest.json.
func Manifest() []byte {
	return bytes.Clone(manifest)
}

// Plugins returns a copy of the embedded shared/plugins.json: the plugins,
// versions, and checksums the site-php image bundles under
// /opt/pressfleet/plugins.
func Plugins() []byte {
	return bytes.Clone(plugins)
}
