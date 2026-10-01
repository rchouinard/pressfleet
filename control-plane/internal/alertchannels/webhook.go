package alertchannels

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Headers of a webhook request.
const (
	// HeaderEvent names the event: firing, resolved, or test.
	HeaderEvent = "X-PressFleet-Event"
	// HeaderDelivery is the message's key, the same across retries.
	HeaderDelivery = "X-PressFleet-Delivery"
	// HeaderTimestamp is the Unix time of the attempt, in seconds.
	HeaderTimestamp = "X-PressFleet-Timestamp"
	// HeaderSignature is "sha256=" and the hex HMAC-SHA256, keyed with the
	// channel's signing secret, of the timestamp, a ".", and the body. It is
	// only sent for channels with a signing secret.
	HeaderSignature = "X-PressFleet-Signature"
)

// maxResponseDrain bounds how much of a response is read, so connections
// can end cleanly without a slow receiver holding a delivery.
const maxResponseDrain = 4 << 10

// Sign returns the value of HeaderSignature for a request.
func Sign(secret string, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// sendWebhook POSTs the message to the URL in the format. It doesn't follow
// redirects, and the client's dialer refuses blocked addresses.
func (s *Service) sendWebhook(ctx context.Context, rawURL, format, signingSecret string, m Message) error {
	body, err := webhookBody(format, m)
	if err != nil {
		return permanent(fmt.Errorf("encode webhook payload: %w", err))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return permanent(errors.New("the webhook URL is invalid"))
	}
	ts := strconv.FormatInt(m.SentAt.Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "PressFleet-Alerts/1")
	req.Header.Set(HeaderEvent, m.Kind)
	req.Header.Set(HeaderDelivery, m.Key)
	req.Header.Set(HeaderTimestamp, ts)
	if signingSecret != "" {
		req.Header.Set(HeaderSignature, Sign(signingSecret, ts, body))
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		// A *url.Error repeats the URL, which is a credential.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		if errors.Is(err, ErrBlockedTarget) {
			return permanent(fmt.Errorf("webhook request: %w", err))
		}
		return fmt.Errorf("webhook request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseDrain))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return permanent(fmt.Errorf("webhook responded with HTTP %d, a redirect, which isn't followed; use the final URL", resp.StatusCode))
	case resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return fmt.Errorf("webhook responded with HTTP %d", resp.StatusCode)
	}
	// Other 4xx responses say the receiver won't take this request, and
	// asking again won't change that.
	return permanent(fmt.Errorf("webhook responded with HTTP %d", resp.StatusCode))
}

// newHTTPClient makes the client webhooks are sent with: no proxy, no
// redirects, and a dialer that refuses local addresses unless allowed.
func newHTTPClient(timeout time.Duration, allowLocal bool, roots *x509.CertPool) *http.Client {
	dialer := guardedDialer(timeout, allowLocal)
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		DisableKeepAlives:     true,
		Proxy:                 nil,
	}
	if roots != nil {
		transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
