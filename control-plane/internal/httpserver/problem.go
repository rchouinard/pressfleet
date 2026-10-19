package httpserver

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// ContentTypeProblem is the media type of RFC 9457 Problem Details.
const ContentTypeProblem = "application/problem+json"

// Problem is an RFC 9457 Problem Details document. It mirrors the Problem
// schema in api/openapi.yaml and implements error, so handlers can return it
// directly to control the response.
type Problem struct {
	// Type is a URI reference identifying the problem type. "about:blank"
	// means the status code says it all.
	Type string `json:"type"`
	// Title is a short summary of the problem type.
	Title string `json:"title"`
	// Status is the HTTP status code.
	Status int `json:"status"`
	// Detail explains this occurrence. It is shown to clients, so it must
	// never contain secrets or internal error text.
	Detail string `json:"detail,omitempty"`
	// Instance is the request path. WriteProblem fills it in.
	Instance string `json:"instance,omitempty"`
	// RequestID correlates the problem with logs. WriteProblem fills it in.
	RequestID string `json:"request_id,omitempty"`
	// Errors lists invalid fields for validation failures.
	Errors []FieldError `json:"errors,omitempty"`
	// RetryAfter is how many seconds clients should wait before trying
	// again, for 429 and 503. The writer also sends it as the Retry-After
	// header, so clients that only see the body (or only the header) get it.
	RetryAfter int `json:"retry_after,omitempty"`
	// Extensions are extra members of a problem type (RFC 9457 §3.2),
	// such as the methods a second-factor-required problem offers. They
	// are written beside the standard members and can't replace them.
	Extensions map[string]any `json:"-"`
}

// MarshalJSON writes the standard members and then the extensions.
func (p Problem) MarshalJSON() ([]byte, error) {
	type standard Problem // without the method, so this doesn't recurse
	body, err := json.Marshal(standard(p))
	if err != nil || len(p.Extensions) == 0 {
		return body, err
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(body, &members); err != nil {
		return nil, err
	}
	for k, v := range p.Extensions {
		if standardMembers[k] {
			continue
		}
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		members[k] = raw
	}
	return json.Marshal(members)
}

// standardMembers are the members Problem defines, which extensions can't
// use even when the standard member is empty and omitted.
var standardMembers = map[string]bool{
	"type": true, "title": true, "status": true, "detail": true, "instance": true,
	"request_id": true, "errors": true, "retry_after": true,
}

// FieldError describes one invalid request field.
type FieldError struct {
	// Field is a JSON Pointer into the request body, or a parameter name.
	Field string `json:"field"`
	// Detail says what is wrong with it.
	Detail string `json:"detail"`
}

// NewProblem returns an "about:blank" problem for status with the standard
// status text as its title.
func NewProblem(status int, detail string) *Problem {
	return &Problem{Type: "about:blank", Title: http.StatusText(status), Status: status, Detail: detail}
}

func (p *Problem) Error() string {
	s := strconv.Itoa(p.Status) + " " + p.Title
	if p.Detail != "" {
		s += ": " + p.Detail
	}
	return s
}

// WriteProblem writes p as the response, filling in the instance and request
// ID from r. p is not modified.
func WriteProblem(w http.ResponseWriter, r *http.Request, p *Problem) {
	out := *p
	if out.Type == "" {
		out.Type = "about:blank"
	}
	if out.Title == "" {
		out.Title = http.StatusText(out.Status)
	}
	if out.Instance == "" {
		out.Instance = r.URL.Path
	}
	if out.RequestID == "" {
		out.RequestID = RequestIDFromContext(r.Context())
	}
	body, err := json.Marshal(out)
	if err != nil {
		// Problem holds strings, ints, and extensions that callers build
		// from plain values; this can't happen.
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Del("Content-Length")
	h.Set("Content-Type", ContentTypeProblem)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(out.Status)
	_, _ = w.Write(append(body, '\n'))
}
