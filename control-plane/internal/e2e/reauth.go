package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/api/client"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// maxProblemBytes bounds how much of a 403 the transport reads to find
// its problem type; problem documents are far smaller.
const maxProblemBytes = 64 << 10

// reauthTransport makes a session client reauthenticate on demand. Tests
// run longer than two_factor.reauthentication_max_age, which the harness
// leaves at its default so the check stays exercised, and their sessions
// take sensitive actions late (deleting sites at the end, say). When the
// control plane refuses a request with the reauthentication-required
// problem type, the transport reauthenticates and sends the request once
// more.
type reauthTransport struct {
	base http.RoundTripper
	// reauthenticate proves the session's user again. It is set once the
	// client it reauthenticates through exists.
	reauthenticate func(context.Context) error
}

func (t *reauthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || t.reauthenticate == nil || !needsReauthentication(resp) {
		return resp, err
	}
	// The first attempt consumed the body; without GetBody (the
	// generated client always sets it) the request can't be sent again.
	if req.Body != nil && req.Body != http.NoBody && req.GetBody == nil {
		return resp, nil
	}
	_ = resp.Body.Close()
	if err := t.reauthenticate(req.Context()); err != nil {
		return nil, fmt.Errorf("e2e: reauthenticating for %s %s: %w", req.Method, req.URL.Path, err)
	}
	retry := req.Clone(req.Context())
	if req.GetBody != nil {
		if retry.Body, err = req.GetBody(); err != nil {
			return nil, fmt.Errorf("e2e: replaying the body of %s %s: %w", req.Method, req.URL.Path, err)
		}
	}
	return t.base.RoundTrip(retry)
}

// needsReauthentication reports whether resp refuses its request until the
// session reauthenticates. It reads a 403's problem document and puts it
// back, so a caller that gets resp still sees the whole body.
func needsReauthentication(resp *http.Response) bool {
	if resp.StatusCode != http.StatusForbidden {
		return false
	}
	if ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); ct != "application/problem+json" {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProblemBytes))
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil {
		return false
	}
	var p struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(body, &p) == nil && p.Type == api.ReauthenticationRequiredProblemType
}

// reauthenticateWithPassword returns the reauthenticate function of a
// session client c for a user without a second factor, as the harness's
// users are.
func reauthenticateWithPassword(c *client.ClientWithResponses, password redact.Secret) func(context.Context) error {
	return func(ctx context.Context) error {
		pw := password.Reveal()
		resp, err := c.ReauthenticateWithResponse(ctx, client.ReauthenticateJSONRequestBody{
			Method: client.ReauthenticationMethodPassword, Password: &pw,
		})
		if err != nil {
			return err
		}
		if resp.JSON200 == nil {
			return fmt.Errorf("status %d: %s", resp.StatusCode(), resp.Body)
		}
		return nil
	}
}
