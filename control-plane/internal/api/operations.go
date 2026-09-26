package api

import (
	"context"
	"fmt"
	"net/http"
	"sort"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/routers"

	apispec "github.com/rchouinard/pressfleet/api"
)

// Operation describes one operation in the OpenAPI document.
type Operation struct {
	// ID is the operationId, e.g. "createSite".
	ID string
	// Method and Path are as written in the document; together they form the
	// http.ServeMux pattern the generated router registers.
	Method, Path string
	// Public reports whether the operation has no security requirement
	// (login and the health probes). Every other operation requires an
	// authenticated caller.
	Public bool

	route *routers.Route
}

// Pattern returns the http.ServeMux pattern for the operation.
func (o *Operation) Pattern() string { return o.Method + " " + o.Path }

// loadSpec parses and validates the embedded OpenAPI document.
func loadSpec() (*openapi3.T, error) {
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(apispec.OpenAPI)
	if err != nil {
		return nil, fmt.Errorf("loading OpenAPI document: %w", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		return nil, fmt.Errorf("validating OpenAPI document: %w", err)
	}
	return doc, nil
}

// operationTable maps ServeMux patterns to operations.
type operationTable map[string]*Operation

func newOperationTable(doc *openapi3.T) operationTable {
	ops := operationTable{}
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			security := doc.Security
			if op.Security != nil {
				security = *op.Security
			}
			o := &Operation{
				ID:     op.OperationID,
				Method: method,
				Path:   path,
				Public: len(security) == 0,
				route:  &routers.Route{Spec: doc, Path: path, PathItem: item, Method: method, Operation: op},
			}
			ops[o.Pattern()] = o
		}
	}
	return ops
}

// forRequest returns the operation the router matched r to. It relies on
// r.Pattern, which http.ServeMux sets before calling the handler.
func (t operationTable) forRequest(r *http.Request) (*Operation, bool) {
	o, ok := t[r.Pattern]
	return o, ok
}

// Operations returns every operation in the OpenAPI document, sorted by path
// and method.
func Operations() ([]Operation, error) {
	doc, err := loadSpec()
	if err != nil {
		return nil, err
	}
	var out []Operation
	for _, o := range newOperationTable(doc) {
		out = append(out, *o)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Method < out[j].Method
	})
	return out, nil
}

type operationKey struct{}

// OperationFromContext returns the operation being served, for use by
// handlers and middleware (for example, to name the action in audit events).
func OperationFromContext(ctx context.Context) (*Operation, bool) {
	o, ok := ctx.Value(operationKey{}).(*Operation)
	return o, ok
}
