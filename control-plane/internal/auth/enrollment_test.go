package auth_test

import (
	"context"
	"errors"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// policyService returns a service on db applying the two-factor policy.
func policyService(db *store.DB, required auth.TwoFactorRequirement) *auth.Service {
	return auth.NewService(db, auth.Options{
		Logger: storetest.Logger(), PasswordParams: cheap,
		TwoFactor: auth.TwoFactorPolicy{Required: required},
	})
}

// credentials are a user's session, API token, and SSH key.
type credentials struct {
	user    store.User
	session redact.Secret
	token   redact.Secret
	sshKey  store.SSHKey
}

// newCredentials creates a user with a session, a full API token, and an
// SSH key, and enrolls them in a second factor after logging in if
// enrolled is set.
func newCredentials(t *testing.T, db *store.DB, svc *auth.Service, email string, role store.Role, enrolled bool) credentials {
	t.Helper()
	ctx := context.Background()
	u := mustCreate(t, svc, email, role)
	res := login(t, svc, email)
	_, tok, err := svc.CreateAPIToken(ctx, u.ID, auth.NewAPIToken{Name: "ci", Scope: store.ScopeFull})
	if err != nil {
		t.Fatal(err)
	}
	k, err := svc.AddSSHKey(ctx, u.ID, auth.NewSSHKey{PublicKey: authorizedKey(ed25519Key(t), "")})
	if err != nil {
		t.Fatal(err)
	}
	if enrolled {
		enroll(t, db, u.ID)
	}
	return credentials{user: u, session: res.Token, token: tok, sshKey: k}
}

// TestEnrollmentRequiredForEachPolicyRoleAndCredential authenticates every
// credential of enrolled and unenrolled users of every role under every
// policy: only sessions of covered, unenrolled users are restricted.
func TestEnrollmentRequiredForEachPolicyRoleAndCredential(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		setup, _ := newService(t, db)
		type user struct {
			role     store.Role
			enrolled bool
			creds    credentials
		}
		var users []user
		for _, role := range store.Roles {
			for _, enrolled := range []bool{false, true} {
				email := string(role) + map[bool]string{false: "-new", true: "-enrolled"}[enrolled] + "@example.com"
				users = append(users, user{role, enrolled, newCredentials(t, db, setup, email, role, enrolled)})
			}
		}
		for _, required := range []auth.TwoFactorRequirement{auth.RequireNone, auth.RequireAdmins, auth.RequireAll} {
			svc := policyService(db, required)
			for _, u := range users {
				covered := required == auth.RequireAll || (required == auth.RequireAdmins && u.role == store.RoleAdmin)
				for _, c := range []struct {
					method       auth.Method
					authenticate func() (auth.Principal, error)
					// action is one every role may take with this credential,
					// outside the enrollment actions.
					action authz.Action
				}{
					{auth.MethodSession, func() (auth.Principal, error) { return svc.AuthenticateSession(ctx, u.creds.session) }, authz.ViewSites},
					{auth.MethodToken, func() (auth.Principal, error) { return svc.AuthenticateToken(ctx, u.creds.token) }, authz.ViewSites},
					{auth.MethodSSHKey, func() (auth.Principal, error) { return svc.RecheckSSHKey(ctx, u.creds.sshKey.ID) }, authz.ConnectSFTP},
				} {
					name := string(required) + "/" + string(u.role) + "/" + map[bool]string{false: "unenrolled", true: "enrolled"}[u.enrolled] + "/" + string(c.method)
					t.Run(name, func(t *testing.T) {
						p, err := c.authenticate()
						if err != nil {
							t.Fatal(err)
						}
						restricted := c.method == auth.MethodSession && covered && !u.enrolled
						if p.Method != c.method || p.EnrollmentRequired != restricted {
							t.Fatalf("principal %+v, want method %s, EnrollmentRequired %v", p, c.method, restricted)
						}
						err = authz.Check(p, c.action, authz.Platform())
						if got := errors.Is(err, authz.ErrEnrollmentRequired); got != restricted {
							t.Errorf("Check(%s) = %v, want enrollment denial %v", c.action, err, restricted)
						}
						// Viewers can't connect over SFTP, enrolled or not.
						roleAllows := c.action != authz.ConnectSFTP || u.role != store.RoleViewer
						if !restricted && (err == nil) != roleAllows {
							t.Errorf("Check(%s) = %v, want allowed %v", c.action, err, roleAllows)
						}
						if c.method != auth.MethodSSHKey {
							if err := authz.Check(p, authz.ViewSelf, authz.User(p.UserID)); err != nil {
								t.Errorf("Check(ViewSelf) = %v", err)
							}
						}
					})
				}
			}
		}
	})
}

// TestEnrollmentRequiredFollowsRoleAndFactors checks that the restriction
// is decided on every request: a promotion to Admin under "admins"
// restricts the user's existing session at once, enrolling lifts it, and
// a demotion lifts it too.
func TestEnrollmentRequiredFollowsRoleAndFactors(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		svc := policyService(db, auth.RequireAdmins)
		mustCreate(t, svc, "admin@example.com", store.RoleAdmin) // so the operator isn't the last Admin when demoted
		op := mustCreate(t, svc, "op@example.com", store.RoleOperator)
		sess := login(t, svc, "op@example.com").Token
		restricted := func(step string, want bool) {
			t.Helper()
			p, err := svc.AuthenticateSession(ctx, sess)
			if err != nil {
				t.Fatalf("%s: %v", step, err)
			}
			if p.EnrollmentRequired != want {
				t.Errorf("%s: EnrollmentRequired = %v, want %v", step, p.EnrollmentRequired, want)
			}
		}
		setRole := func(role store.Role) {
			t.Helper()
			if _, err := svc.UpdateUser(ctx, op.ID, auth.UserChanges{Role: &role}); err != nil {
				t.Fatal(err)
			}
		}
		restricted("operator", false)
		setRole(store.RoleAdmin)
		restricted("promoted to admin", true)
		setRole(store.RoleOperator)
		restricted("demoted", false)
		setRole(store.RoleAdmin)
		enroll(t, db, op.ID)
		restricted("enrolled admin", false)
	})
}
