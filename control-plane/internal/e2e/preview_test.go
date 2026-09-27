package e2e_test

import (
	"io"
	"net/http"
	"slices"
	"testing"
	"time"
)

// ingressWait bounds how long a change of a site's routes may take to show
// through Traefik. Traefik picks up container changes from Docker events
// within a second or two; this leaves room for a slow CI runner.
const ingressWait = 30 * time.Second

// ingressGet sends one GET through Traefik with the ingress client and
// returns the response with its body read.
func ingressGet(t *testing.T, hc *http.Client, url string, header http.Header) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp, string(body)
}

// waitIngress repeats a GET through Traefik until it answers with one of
// the wanted statuses, and fails the test if that takes longer than
// ingressWait. Routes appear, change, and disappear a moment after the
// containers or files behind them change.
func waitIngress(t *testing.T, hc *http.Client, url string, header http.Header, want ...int) (*http.Response, string) {
	t.Helper()
	deadline := time.Now().Add(ingressWait)
	for {
		resp, body := ingressGet(t, hc, url, header)
		if slices.Contains(want, resp.StatusCode) {
			t.Logf("GET %s: %s", url, resp.Status)
			return resp, body
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s: status %d after %s, want %v:\n%.500s", url, resp.StatusCode, ingressWait, want, body)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
