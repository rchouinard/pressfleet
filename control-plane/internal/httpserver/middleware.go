package httpserver

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

// Middleware wraps an http.Handler.
type Middleware func(http.Handler) http.Handler

// Chain applies middleware so that the first one listed is the outermost.
func Chain(h http.Handler, mw ...Middleware) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}

// HeaderRequestID carries the request ID in requests and responses.
const HeaderRequestID = "X-Request-ID"

// maxRequestIDLen bounds client-supplied request IDs so they can't bloat logs.
const maxRequestIDLen = 128

type ctxKey int

const (
	requestIDKey ctxKey = iota
	loggerKey
)

// RequestIDFromContext returns the request ID stored by the RequestID
// middleware, or "" if there is none.
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// LoggerFromContext returns the request-scoped logger (which includes the
// request ID) stored by the AccessLog middleware, or fallback if there is
// none.
func LoggerFromContext(ctx context.Context, fallback *slog.Logger) *slog.Logger {
	if l, ok := ctx.Value(loggerKey).(*slog.Logger); ok {
		return l
	}
	return fallback
}

// RequestID accepts a well-formed X-Request-ID from the client (for example,
// one set by the edge proxy) or generates one, stores it in the request
// context, and echoes it in the response.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(HeaderRequestID)
		if !validRequestID(id) {
			id = rand.Text()
		}
		w.Header().Set(HeaderRequestID, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

// validRequestID accepts short IDs made of URL-safe characters, so a client
// can't inject control characters or oversized values into logs.
func validRequestID(id string) bool {
	if id == "" || len(id) > maxRequestIDLen {
		return false
	}
	for _, c := range []byte(id) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.', c == ':':
		default:
			return false
		}
	}
	return true
}

// AccessLog logs one line per request and stores a request-scoped logger in
// the context. It deliberately logs only the method, path, and response
// metadata: never headers (Authorization, Cookie), query strings, or bodies,
// any of which can carry credentials.
func AccessLog(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			reqLog := log.With("request_id", RequestIDFromContext(r.Context()))
			rec := &statusRecorder{ResponseWriter: w}
			r = r.WithContext(context.WithValue(r.Context(), loggerKey, reqLog))
			next.ServeHTTP(rec, r)
			reqLog.LogAttrs(r.Context(), slog.LevelInfo, "http request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("route", r.Pattern),
				slog.Int("status", rec.status()),
				slog.Int64("bytes", rec.bytes),
				slog.Duration("duration", time.Since(start)),
				slog.String("remote_addr", r.RemoteAddr),
			)
		})
	}
}

// Recover turns a panic in a handler into a 500 Problem Details response and
// logs it with a stack trace.
func Recover(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := &statusRecorder{ResponseWriter: w}
			defer func() {
				p := recover()
				if p == nil {
					return
				}
				if p == http.ErrAbortHandler { //nolint:errorlint // sentinel panic value, compared as net/http does
					panic(p)
				}
				LoggerFromContext(r.Context(), log).Error("panic serving request",
					"panic", fmt.Sprint(p), "stack", string(debug.Stack()),
					"method", r.Method, "path", r.URL.Path)
				if rec.wroteHeader {
					// Too late for a problem response; abort so the client
					// sees a truncated response rather than a partial success.
					panic(http.ErrAbortHandler)
				}
				WriteProblem(rec, r, NewProblem(http.StatusInternalServerError, "An unexpected error occurred."))
			}()
			next.ServeHTTP(rec, r)
		})
	}
}

// MaxBodyBytes limits request bodies to n bytes. Reading past the limit fails
// with *http.MaxBytesError, which API error handling maps to 413. The few
// operations that take larger bodies raise the limit with SetBodyLimit.
func MaxBodyBytes(n int64) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil && r.Body != http.NoBody {
				r = r.WithContext(context.WithValue(r.Context(), unlimitedBodyKey{}, r.Body))
				r.Body = http.MaxBytesReader(w, r.Body, n)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// unlimitedBodyKey holds the request body as it was before MaxBodyBytes
// limited it.
type unlimitedBodyKey struct{}

// SetBodyLimit replaces the limit MaxBodyBytes put on r's body with n, for
// an operation that takes large bodies, such as an upload chunk. Call it
// before anything reads the body. Requests MaxBodyBytes didn't limit are
// left alone.
func SetBodyLimit(w http.ResponseWriter, r *http.Request, n int64) {
	if body, ok := r.Context().Value(unlimitedBodyKey{}).(io.ReadCloser); ok {
		r.Body = http.MaxBytesReader(w, body, n)
	}
}

// statusRecorder records the status code and body size of a response.
type statusRecorder struct {
	http.ResponseWriter
	code        int
	bytes       int64
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.code, s.wroteHeader = code, true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.code, s.wroteHeader = http.StatusOK, true
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *statusRecorder) status() int {
	if !s.wroteHeader {
		// Nothing written: net/http sends 200 when the handler returns.
		return http.StatusOK
	}
	return s.code
}
