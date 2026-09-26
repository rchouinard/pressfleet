// Package api embeds the OpenAPI document so the control plane can validate
// requests against it and derive per-operation metadata (such as which
// operations require authentication) from the single source of truth.
//
// The file lives next to openapi.yaml because go:embed can't reach outside a
// package directory.
package api

import _ "embed"

// OpenAPI is the raw OpenAPI 3.1 document (openapi.yaml).
//
//go:embed openapi.yaml
var OpenAPI []byte
