package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/rchouinard/pressfleet/control-plane/internal/adminlogin"
	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
	"github.com/rchouinard/pressfleet/control-plane/internal/httpserver"
)

// One-click WordPress admin login (#129, design doc §5.9).

// errNoAdminLogin is a wiring bug: serve always passes the service.
var errNoAdminLogin = errors.New("api: no admin login service configured")

// ChooseUserProblemType is the problem type of the 409 that asks the
// caller to choose which administrator to log in as. It links to the
// description of the login flow, so clients can tell it from the other
// 409s of the same call (site not running, in maintenance mode).
const ChooseUserProblemType = "https://github.com/rchouinard/pressfleet/blob/main/docs/pressfleet_architecture_and_implementation_document.md#59-dashboard-login-to-wordpress"

// CreateSiteAdminLogin issues a login token for the calling user. The audit
// event records the WordPress user and the token's jti, never the token.
func (h *Handlers) CreateSiteAdminLogin(ctx context.Context, req gen.CreateSiteAdminLoginRequestObject) (gen.CreateSiteAdminLoginResponseObject, error) {
	p, err := h.authorize(ctx, authz.AdminLogin, authz.Site(req.SiteID))
	if err != nil {
		return nil, err
	}
	if h.adminLogin == nil {
		return nil, errNoAdminLogin
	}
	user := ""
	if req.Body != nil && req.Body.User != nil {
		user = *req.Body.User
	}
	l, err := h.adminLogin.Issue(ctx, req.SiteID, user, p.UserID)
	if err != nil {
		return nil, adminLoginError(err)
	}
	setAuditDetails(ctx, map[string]string{"wp_user": l.User, "jti": l.JTI})
	noStore := "no-store"
	return gen.CreateSiteAdminLogin200JSONResponse{
		Body:    gen.AdminLogin{URL: l.URL, Token: l.Token, User: l.User, ExpiresAt: l.ExpiresAt},
		Headers: gen.CreateSiteAdminLogin200ResponseHeaders{CacheControl: &noStore},
	}, nil
}

// ListSiteAdministrators lists the site's WordPress administrators.
func (h *Handlers) ListSiteAdministrators(ctx context.Context, req gen.ListSiteAdministratorsRequestObject) (gen.ListSiteAdministratorsResponseObject, error) {
	if _, err := h.authorize(ctx, authz.AdminLogin, authz.Site(req.SiteID)); err != nil {
		return nil, err
	}
	if h.adminLogin == nil {
		return nil, errNoAdminLogin
	}
	admins, err := h.adminLogin.Administrators(ctx, req.SiteID)
	if err != nil {
		return nil, adminLoginError(err)
	}
	items := make([]gen.SiteAdministrator, len(admins))
	for i, a := range admins {
		items[i] = gen.SiteAdministrator{Login: a.Login, DisplayName: a.DisplayName, Email: a.Email, Default: a.Default}
	}
	noStore := "no-store"
	return gen.ListSiteAdministrators200JSONResponse{
		Body:    gen.SiteAdministratorList{Items: items},
		Headers: gen.ListSiteAdministrators200ResponseHeaders{CacheControl: &noStore},
	}, nil
}

// adminLoginError maps the admin login service's errors to responses.
func adminLoginError(err error) error {
	switch {
	case errors.Is(err, adminlogin.ErrSiteNotFound):
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	case errors.Is(err, adminlogin.ErrUnavailable):
		return fmt.Errorf("%w: %w", ErrConflict, err)
	case errors.Is(err, adminlogin.ErrChooseUser):
		p := httpserver.NewProblem(http.StatusConflict,
			"The site has no usable default administrator. List the administrators and send the request again with the user to log in as.")
		p.Type, p.Title = ChooseUserProblemType, "Choose an administrator"
		return p
	case errors.Is(err, adminlogin.ErrNotAdministrator):
		return &ValidationError{Fields: []httpserver.FieldError{{Field: "/user", Detail: "not an administrator of this site"}}}
	}
	return err
}
