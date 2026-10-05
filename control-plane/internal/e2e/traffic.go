package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"
)

// trafficInterval is the time between a traffic probe's requests.
const trafficInterval = 250 * time.Millisecond

// TrafficProbe requests a URL over and over in the background, as a
// visitor would, and counts the requests that failed: a transport error
// or a status other than 200. Tests use it to check that something, such
// as upgrading the control plane, doesn't interrupt a site.
type TrafficProbe struct {
	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.Mutex
	ok       int
	failures []string
}

// ProbeTraffic starts probing url with hc, such as an IngressClient, until
// Stop. The test fails if Stop isn't called.
func ProbeTraffic(t *testing.T, hc *http.Client, url string) *TrafficProbe {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	p := &TrafficProbe{cancel: cancel, done: make(chan struct{})}
	go p.run(ctx, hc, url)
	t.Cleanup(func() {
		select {
		case <-p.done:
		default:
			p.Stop()
			t.Error("e2e: a traffic probe wasn't stopped")
		}
	})
	return p
}

func (p *TrafficProbe) run(ctx context.Context, hc *http.Client, url string) {
	defer close(p.done)
	tick := time.NewTicker(trafficInterval)
	defer tick.Stop()
	for {
		failure := p.request(ctx, hc, url)
		if ctx.Err() != nil {
			return // the cancellation, not the site, failed the request
		}
		p.mu.Lock()
		if failure == "" {
			p.ok++
		} else {
			p.failures = append(p.failures, time.Now().Format("15:04:05.000 ")+failure)
		}
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// request makes one request and describes its failure, or returns "".
func (p *TrafficProbe) request(ctx context.Context, hc *http.Client, url string) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err.Error()
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return fmt.Sprintf("reading the body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		return resp.Status
	}
	return ""
}

// Stop stops probing and returns the number of successful requests and a
// description of each failed one.
func (p *TrafficProbe) Stop() (ok int, failures []string) {
	p.cancel()
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ok, p.failures
}
