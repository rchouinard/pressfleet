package httpserver

import "net/http"

// ProblemRouter serves requests with mux, but replaces the plain-text 404
// and 405 responses that http.ServeMux produces for unmatched requests with
// Problem Details. Requests that match a route are served unchanged, so
// handlers keep full control of their own 404s.
func ProblemRouter(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern != "" {
			mux.ServeHTTP(w, r)
			return
		}
		mux.ServeHTTP(&unmatchedWriter{ResponseWriter: w, r: r}, r)
	})
}

// unmatchedWriter intercepts ServeMux's built-in 404 and 405 responses.
// Other responses (such as path-cleaning redirects) pass through.
type unmatchedWriter struct {
	http.ResponseWriter
	r           *http.Request
	wroteHeader bool
	intercepted bool
}

func (u *unmatchedWriter) WriteHeader(code int) {
	if u.wroteHeader {
		return
	}
	u.wroteHeader = true
	switch code {
	case http.StatusNotFound:
		u.intercepted = true
		WriteProblem(u.ResponseWriter, u.r, NewProblem(code, "No resource exists at this path."))
	case http.StatusMethodNotAllowed:
		// ServeMux has already set the Allow header.
		u.intercepted = true
		WriteProblem(u.ResponseWriter, u.r, NewProblem(code, "Method "+u.r.Method+" is not allowed for this resource."))
	default:
		u.ResponseWriter.WriteHeader(code)
	}
}

func (u *unmatchedWriter) Write(b []byte) (int, error) {
	if !u.wroteHeader {
		u.WriteHeader(http.StatusOK)
	}
	if u.intercepted {
		// Discard ServeMux's plain-text body.
		return len(b), nil
	}
	return u.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (u *unmatchedWriter) Unwrap() http.ResponseWriter { return u.ResponseWriter }
