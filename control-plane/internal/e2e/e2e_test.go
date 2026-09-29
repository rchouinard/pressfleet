package e2e_test

import (
	"net/http"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/client"
	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// TestAPI boots the platform once and checks authentication end to end:
// sessions, CSRF on unsafe requests, bearer tokens, and 401 for everyone
// else.
func TestAPI(t *testing.T) {
	p := e2e.Start(t)

	t.Run("current user", func(t *testing.T) {
		resp, err := p.Client.GetCurrentUserWithResponse(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		me := resp.JSON200
		if me == nil {
			t.Fatalf("status %d: %s", resp.StatusCode(), resp.Body)
		}
		if string(me.Email) != p.AdminEmail || me.Role != client.Admin || me.AuthMethod != client.AuthMethodSession {
			t.Errorf("me = %+v", me)
		}
	})

	t.Run("list jobs", func(t *testing.T) {
		resp, err := p.Client.ListJobsWithResponse(t.Context(), &client.ListJobsParams{})
		if err != nil {
			t.Fatal(err)
		}
		if resp.JSON200 == nil {
			t.Fatalf("status %d: %s", resp.StatusCode(), resp.Body)
		}
	})

	t.Run("bearer token", func(t *testing.T) {
		// Creating a token is an unsafe request, so it also proves the
		// session client sends its CSRF token.
		created, err := p.Client.CreateAPITokenWithResponse(t.Context(), client.CreateAPITokenJSONRequestBody{
			Name: "e2e", Scope: client.TokenScopeReadOnly,
		})
		if err != nil {
			t.Fatal(err)
		}
		if created.JSON201 == nil {
			t.Fatalf("create token: status %d: %s", created.StatusCode(), created.Body)
		}
		bearer := p.BearerClient(t, redact.Secret(created.JSON201.Token))
		resp, err := bearer.GetCurrentUserWithResponse(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if resp.JSON200 == nil || resp.JSON200.AuthMethod != client.AuthMethodToken {
			t.Fatalf("status %d: %s", resp.StatusCode(), resp.Body)
		}
	})

	t.Run("unauthenticated", func(t *testing.T) {
		clients := map[string]*client.ClientWithResponses{
			"anonymous":  p.NewClient(t),
			"bad bearer": p.BearerClient(t, "pft_notarealtoken"),
		}
		for name, c := range clients {
			t.Run(name, func(t *testing.T) {
				me, err := c.GetCurrentUserWithResponse(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				jobs, err := c.ListJobsWithResponse(t.Context(), &client.ListJobsParams{})
				if err != nil {
					t.Fatal(err)
				}
				for op, resp := range map[string]*http.Response{"me": me.HTTPResponse, "jobs": jobs.HTTPResponse} {
					if resp.StatusCode != http.StatusUnauthorized {
						t.Errorf("%s: status %d, want 401", op, resp.StatusCode)
					}
					if resp.Header.Get("WWW-Authenticate") == "" {
						t.Errorf("%s: no WWW-Authenticate header", op)
					}
				}
			})
		}

		wrong := "not-the-password"
		resp, err := p.NewClient(t).LoginWithResponse(t.Context(), client.LoginJSONRequestBody{
			Email: client.Email(p.AdminEmail), Password: &wrong,
		})
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode() != http.StatusUnauthorized {
			t.Errorf("login with a wrong password: status %d, want 401", resp.StatusCode())
		}
	})
}
