package adminlogin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Errors callers branch on.
var (
	// ErrSiteNotFound means the site doesn't exist.
	ErrSiteNotFound = errors.New("site not found")
	// ErrUnavailable means the site can't take a login now: it isn't
	// running, or (for a login) it is in maintenance mode.
	ErrUnavailable = errors.New("site is unavailable for login")
	// ErrChooseUser means the caller named no user and the site has no
	// usable default administrator, so the caller must pick one from
	// [Service.Administrators].
	ErrChooseUser = errors.New("choose an administrator")
	// ErrNotAdministrator means the named user doesn't exist on the site or
	// isn't an administrator.
	ErrNotAdministrator = errors.New("user is not an administrator of the site")
)

// WPRunner runs WP-CLI in a site; provisioner.Provisioner implements it.
type WPRunner interface {
	ExecWPCLI(ctx context.Context, siteID string, args []string) (string, error)
}

// Service issues login tokens for sites (design doc §5.9).
type Service struct {
	db     *store.DB
	wp     WPRunner
	signer *Signer
}

// NewService returns a Service that reads sites from db, lists their
// administrators with wp, and signs tokens with signer.
func NewService(db *store.DB, wp WPRunner, signer *Signer) *Service {
	return &Service{db: db, wp: wp, signer: signer}
}

// Administrator is a WordPress administrator of a site.
type Administrator struct {
	Login       string
	DisplayName string
	Email       string
	// Default is the administrator the platform created with the site.
	Default bool
}

// Login is an issued token and where to submit it.
type Login struct {
	// URL is the site's login URL for the token.
	URL string
	// Token is the secret token. Never log or audit it.
	Token string
	// User is the WordPress user's login the token logs in.
	User string
	// JTI is the token's ID, for the audit event.
	JTI       string
	ExpiresAt time.Time
}

// loginPath is the site's login endpoint, handled by the must-use plugin.
const loginPath = "/wp-login.php?action=pressfleet-login"

// wpUser is an entry of `wp user list --format=json`.
type wpUser struct {
	Login       string `json:"user_login"`
	DisplayName string `json:"display_name"`
	Email       string `json:"user_email"`
}

// runningSite returns the site if it exists and is running.
func (s *Service) runningSite(ctx context.Context, siteID string) (store.Site, error) {
	site, err := store.NewSites(s.db).Get(ctx, siteID)
	if errors.Is(err, store.ErrNotFound) {
		return store.Site{}, fmt.Errorf("site %s: %w", siteID, ErrSiteNotFound)
	}
	if err != nil {
		return store.Site{}, err
	}
	if site.Status != store.SiteRunning {
		return store.Site{}, fmt.Errorf("site %s is %s, not running: %w", siteID, site.Status, ErrUnavailable)
	}
	return site, nil
}

// Administrators lists the site's WordPress administrators, the platform's
// default one marked. The site must be running.
func (s *Service) Administrators(ctx context.Context, siteID string) ([]Administrator, error) {
	site, err := s.runningSite(ctx, siteID)
	if err != nil {
		return nil, err
	}
	return s.administrators(ctx, site)
}

func (s *Service) administrators(ctx context.Context, site store.Site) ([]Administrator, error) {
	out, err := s.wp.ExecWPCLI(ctx, site.ID, []string{
		"user", "list", "--role=administrator", "--fields=user_login,display_name,user_email", "--format=json",
	})
	if errors.Is(err, provisioner.ErrSiteNotRunning) {
		return nil, fmt.Errorf("site %s is not running: %w", site.ID, ErrUnavailable)
	}
	if err != nil {
		return nil, fmt.Errorf("listing administrators: %w", err)
	}
	var users []wpUser
	if strings.TrimSpace(out) != "" {
		if err := json.Unmarshal([]byte(out), &users); err != nil {
			return nil, fmt.Errorf("listing administrators: unexpected output from WP-CLI: %w", err)
		}
	}
	admins := make([]Administrator, len(users))
	for i, u := range users {
		admins[i] = Administrator{
			Login: u.Login, DisplayName: u.DisplayName, Email: u.Email,
			Default: site.AdminUsername != "" && u.Login == site.AdminUsername,
		}
	}
	return admins, nil
}

// Issue signs a login token for actorID (the platform user) to log in to
// the site as the WordPress user. With an empty user it uses the site's
// default administrator, or returns [ErrChooseUser] when there is none
// (imported site, user deleted or demoted). The site must be running and
// not in maintenance mode.
func (s *Service) Issue(ctx context.Context, siteID, user, actorID string) (Login, error) {
	site, err := s.runningSite(ctx, siteID)
	if err != nil {
		return Login{}, err
	}
	if site.Maintenance {
		return Login{}, fmt.Errorf("site %s is in maintenance mode: %w", siteID, ErrUnavailable)
	}
	admins, err := s.administrators(ctx, site)
	if err != nil {
		return Login{}, err
	}
	var login string
	for _, a := range admins {
		if (user == "" && a.Default) || (user != "" && strings.EqualFold(a.Login, user)) {
			login = a.Login
			break
		}
	}
	switch {
	case login == "" && user == "":
		return Login{}, ErrChooseUser
	case login == "":
		return Login{}, ErrNotAdministrator
	}
	token, jti, exp, err := s.signer.Sign(site.ID, login, actorID)
	if err != nil {
		return Login{}, fmt.Errorf("signing login token: %w", err)
	}
	return Login{URL: sites.URL(site) + loginPath, Token: token, User: login, JTI: jti, ExpiresAt: exp}, nil
}
