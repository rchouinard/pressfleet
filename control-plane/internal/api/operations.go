package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/routers"

	apispec "github.com/rchouinard/pressfleet/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
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
	// Action is the authorization action the operation's handler checks
	// (package authz), or empty for public operations.
	Action authz.Action
	// AltActions are further actions the handler may check instead of
	// Action, for operations whose action depends on their target (see
	// operationAltActions).
	AltActions []authz.Action
	// BinaryBody reports whether the request body is raw bytes
	// (application/octet-stream), which is streamed to the handler rather
	// than validated.
	BinaryBody bool

	route *routers.Route
}

// Pattern returns the http.ServeMux pattern for the operation.
func (o *Operation) Pattern() string { return o.Method + " " + o.Path }

// allows reports whether the operation's handler may check action.
func (o *Operation) allows(action authz.Action) bool {
	return action == o.Action || slices.Contains(o.AltActions, action)
}

// binaryMediaType is the content type of raw request bodies, such as
// upload chunks.
const binaryMediaType = "application/octet-stream"

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
				ID:         op.OperationID,
				Method:     method,
				Path:       path,
				Public:     len(security) == 0,
				Action:     operationActions[op.OperationID],
				AltActions: operationAltActions[op.OperationID],
				route:      &routers.Route{Spec: doc, Path: path, PathItem: item, Method: method, Operation: op},
			}
			if op.RequestBody != nil && op.RequestBody.Value != nil {
				o.BinaryBody = op.RequestBody.Value.Content.Get(binaryMediaType) != nil
			}
			ops[o.Pattern()] = o
		}
	}
	return ops
}

// checkActions reports protected operations without a known authorization
// action, and declared actions for operations that don't exist or are
// public: the API must not serve an operation nobody decided who may call.
func (t operationTable) checkActions() error {
	var errs []error
	declared := map[string]bool{}
	for _, o := range t {
		declared[o.ID] = true
		switch {
		case o.Public && o.Action != "":
			errs = append(errs, fmt.Errorf("public operation %s must not declare an authorization action", o.ID))
		case !o.Public && !o.Action.Known():
			errs = append(errs, fmt.Errorf("operation %s has no known authorization action; add it to operationActions", o.ID))
		}
		for _, a := range o.AltActions {
			if o.Public || !a.Known() {
				errs = append(errs, fmt.Errorf("operation %s declares alternative action %q, which is unknown or on a public operation", o.ID, a))
			}
		}
	}
	for id := range operationActions {
		if !declared[id] {
			errs = append(errs, fmt.Errorf("operationActions declares %s, which isn't in the OpenAPI document", id))
		}
	}
	for id := range operationAltActions {
		if !declared[id] {
			errs = append(errs, fmt.Errorf("operationAltActions declares %s, which isn't in the OpenAPI document", id))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("api: %w", err)
	}
	return nil
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
