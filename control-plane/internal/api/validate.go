package api

import (
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"

	"github.com/rchouinard/pressfleet/control-plane/internal/httpserver"
)

// validationOptions configures request validation. Authentication is
// handled by our own middleware, so the validator's security check is a
// no-op.
var validationOptions = &openapi3filter.Options{
	MultiError:         true,
	AuthenticationFunc: openapi3filter.NoopAuthenticationFunc,
}

// validateRequests checks parameters and bodies against the OpenAPI
// document before the generated code decodes them, so handlers only receive
// requests that match the schema. Malformed requests get 400 (413 if too
// large, 415 for the wrong content type); well-formed bodies that violate the
// schema get 422 with one error per field.
func validateRequests(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			op, ok := OperationFromContext(r.Context())
			if !ok {
				next.ServeHTTP(w, r)
				return
			}
			input := &openapi3filter.RequestValidationInput{
				Request:    r,
				PathParams: pathParams(r, op),
				Route:      op.route,
				Options:    validationOptions,
			}
			if err := openapi3filter.ValidateRequest(r.Context(), input); err != nil {
				writeError(log, w, r, validationProblem(err))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// pathParams extracts the operation's path parameters as matched by
// http.ServeMux.
func pathParams(r *http.Request, op *Operation) map[string]string {
	params := map[string]string{}
	add := func(ps openapi3.Parameters) {
		for _, p := range ps {
			if p.Value != nil && p.Value.In == openapi3.ParameterInPath {
				params[p.Value.Name] = r.PathValue(p.Value.Name)
			}
		}
	}
	add(op.route.PathItem.Parameters)
	add(op.route.Operation.Parameters)
	return params
}

// validationProblem converts validator errors into a single problem. The
// most fundamental failure wins: an oversized body (413), then the wrong
// content type (415), then malformed input (400), then schema violations
// (422). Messages come from the schema, never from the submitted values, so
// secrets such as passwords are not echoed back.
func validationProblem(err error) error {
	var (
		fields      []httpserver.FieldError
		malformed   bool
		unsupported bool
	)
	for _, e := range flatten(err) {
		var mbe *http.MaxBytesError
		if errors.As(e, &mbe) {
			return e
		}
		var re *openapi3filter.RequestError
		if !errors.As(e, &re) {
			malformed = true
			fields = append(fields, httpserver.FieldError{Field: "", Detail: e.Error()})
			continue
		}
		switch {
		case re.Parameter != nil:
			malformed = true
			fields = append(fields, httpserver.FieldError{Field: re.Parameter.Name, Detail: parameterDetail(re)})
		case strings.HasPrefix(re.Reason, "header Content-Type has unexpected value"):
			unsupported = true
		case errors.Is(re.Err, openapi3filter.ErrInvalidRequired):
			malformed = true
			fields = append(fields, httpserver.FieldError{Field: "", Detail: "request body is required"})
		default:
			schemaErrs := schemaErrors(re.Err)
			if len(schemaErrs) == 0 {
				malformed = true
				fields = append(fields, httpserver.FieldError{Field: "", Detail: "request body is not valid JSON"})
				continue
			}
			for _, se := range schemaErrs {
				fields = append(fields, schemaFieldErrors(se)...)
			}
		}
	}
	switch {
	case unsupported:
		return httpserver.NewProblem(http.StatusUnsupportedMediaType, "The request body must be application/json.")
	case malformed:
		p := httpserver.NewProblem(http.StatusBadRequest, "The request is malformed.")
		p.Errors = fields
		return p
	default:
		p := httpserver.NewProblem(http.StatusUnprocessableEntity, "The request failed validation.")
		p.Errors = fields
		return p
	}
}

// parameterDetail describes a parameter error without echoing its value.
func parameterDetail(re *openapi3filter.RequestError) string {
	if ses := schemaErrors(re.Err); len(ses) > 0 {
		if fes := schemaFieldErrors(ses[0]); len(fes) > 0 {
			return fes[0].Detail
		}
	}
	if errors.Is(re.Err, openapi3filter.ErrInvalidRequired) {
		return "is required"
	}
	return "has an invalid format"
}

// flatten expands openapi3.MultiError trees into their leaf errors. It
// deliberately doesn't use errors.As, which would look through a
// RequestError into the MultiError it wraps and lose the RequestError.
func flatten(err error) []error {
	if me, ok := err.(openapi3.MultiError); ok { //nolint:errorlint // only a MultiError at this level, see above

		var out []error
		for _, e := range me {
			out = append(out, flatten(e)...)
		}
		return out
	}
	return []error{err}
}

// schemaErrors returns the schema violations in err, if any.
func schemaErrors(err error) []*openapi3.SchemaError {
	var out []*openapi3.SchemaError
	for _, e := range flatten(err) {
		var se *openapi3.SchemaError
		if errors.As(e, &se) {
			out = append(out, se)
		}
	}
	return out
}

// kin-openapi validates OpenAPI 3.1 documents with a JSON Schema 2020-12
// validator, falling back to its built-in validator for schemas the former
// can't compile. The two phrase errors differently, and the 2020-12 one packs
// several errors into one message ("- at '/path': message" lines) and quotes
// offending string values. These patterns normalize both.
var (
	jsonschemaLine    = regexp.MustCompile(`^\s*- at '([^']*)': (.*)$`)
	missingProperty   = regexp.MustCompile(`^(?:missing propert(?:y|ies) (.+)|property "([^"]+)" is missing)$`)
	unknownProperty   = regexp.MustCompile(`^(?:additional ?[pP]ropert(?:y|ies) (.+) not allowed|property "([^"]+)" is unsupported)$`)
	leadingQuotedText = regexp.MustCompile(`^'(?:[^'\\]|\\.)*'`)
)

// schemaFieldErrors converts a schema error into field errors with JSON
// Pointers and messages that never contain the submitted value.
func schemaFieldErrors(se *openapi3.SchemaError) []httpserver.FieldError {
	base := pointer(se.JSONPointer())
	var out []httpserver.FieldError
	for line := range strings.SplitSeq(se.Reason, "\n") {
		if m := jsonschemaLine.FindStringSubmatch(line); m != nil {
			out = append(out, fieldErrors(base+m[1], m[2])...)
		}
	}
	if len(out) == 0 {
		out = fieldErrors(base, se.Reason)
	}
	return slices.Compact(out)
}

// fieldErrors turns one validator message about field into field errors.
func fieldErrors(field, msg string) []httpserver.FieldError {
	perProperty := func(names []string, detail string) []httpserver.FieldError {
		var out []httpserver.FieldError
		for _, n := range names {
			for name := range strings.SplitSeq(n, ",") {
				name = strings.Trim(strings.TrimSpace(name), `'"`)
				if name != "" {
					out = append(out, httpserver.FieldError{Field: field + pointer([]string{name}), Detail: detail})
				}
			}
		}
		return out
	}
	if m := missingProperty.FindStringSubmatch(msg); m != nil {
		return perProperty(m[1:], "is required")
	}
	if m := unknownProperty.FindStringSubmatch(msg); m != nil {
		return perProperty(m[1:], "is not allowed")
	}
	// The 2020-12 validator starts some messages with the offending value,
	// e.g. "'hunter2' does not match pattern ...".
	msg = leadingQuotedText.ReplaceAllString(msg, "value")
	return []httpserver.FieldError{{Field: field, Detail: msg}}
}

// pointer formats path segments as an RFC 6901 JSON Pointer.
func pointer(segments []string) string {
	var b strings.Builder
	for _, s := range segments {
		b.WriteByte('/')
		b.WriteString(strings.NewReplacer("~", "~0", "/", "~1").Replace(s))
	}
	return b.String()
}
