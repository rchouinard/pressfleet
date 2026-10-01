package alertchannelstest

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// Request is one request a [Webhook] received.
type Request struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

// Webhook is a webhook receiver that records requests and answers with a
// status a test chooses.
type Webhook struct {
	// URL is the receiver's address, with the path "/hook/secret-token".
	URL string

	mu       sync.Mutex
	requests []Request
	status   int
	redirect string
}

// NewWebhook starts a receiver that answers 200, closed when the test ends.
// Its address is on loopback, so a service under test needs
// AllowLocalTargets.
func NewWebhook(t testing.TB) *Webhook {
	t.Helper()
	w := &Webhook{status: http.StatusOK}
	ts := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.mu.Lock()
		w.requests = append(w.requests, Request{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
		status, redirect := w.status, w.redirect
		w.mu.Unlock()
		if redirect != "" {
			http.Redirect(rw, r, redirect, status)
			return
		}
		rw.WriteHeader(status)
	}))
	t.Cleanup(ts.Close)
	w.URL = ts.URL + "/hook/secret-token"
	return w
}

// SetStatus sets the status the receiver answers with.
func (w *Webhook) SetStatus(status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.status = status
}

// SetRedirect makes the receiver answer with a redirect (the status set by
// SetStatus, which should be 3xx) to location.
func (w *Webhook) SetRedirect(location string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.redirect = location
}

// Requests returns the requests received so far.
func (w *Webhook) Requests() []Request {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]Request(nil), w.requests...)
}
