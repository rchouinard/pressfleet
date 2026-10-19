package authz_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// designTable restates the design doc §4.6 table independently of the
// package's rules: which roles may take each action, and whether it
// changes state (so read-only tokens can't take it).
var designTable = map[authz.Action]struct {
	admin, operator, viewer, write bool
}{
	// View sites, client groups, jobs, backups, metrics, logs, and alerts.
	authz.ViewSites:        {true, true, true, false},
	authz.ViewClientGroups: {true, true, true, false},
	authz.ViewJobs:         {true, true, true, false},
	authz.ViewBackups:      {true, true, true, false},
	authz.ViewMonitoring:   {true, true, true, false},
	// Create sites; change site settings; go-live and domains; start/stop;
	// maintenance mode; cache purge.
	authz.CreateSite:              {true, true, false, true},
	authz.RetrieveSiteCredentials: {true, true, false, true},
	authz.UpdateSite:              {true, true, false, true},
	authz.ManageSiteDomains:       {true, true, false, true},
	authz.StartSite:               {true, true, false, true},
	authz.StopSite:                {true, true, false, true},
	authz.SetSiteMaintenance:      {true, true, false, true},
	authz.PurgeSiteCache:          {true, true, false, true},
	authz.ImportSite:              {true, true, false, true},
	// Deploy code; connect over SFTP; export the database; open read-only
	// database sessions. Uploads for imports and deployments need what the
	// import or deployment needs.
	authz.ConnectSFTP:            {true, true, false, true},
	authz.DeployCode:             {true, true, false, true},
	authz.AdminLogin:             {true, true, false, true},
	authz.UploadForImport:        {true, true, false, true},
	authz.UploadForDeploy:        {true, true, false, true},
	authz.ExportDatabase:         {true, true, false, true},
	authz.ExportSite:             {true, true, false, true},
	authz.DownloadDatabaseExport: {true, true, false, true},
	authz.OpenDatabaseSession:    {true, true, false, true},
	authz.EndDatabaseSession:     {true, true, false, true},
	authz.TunnelDatabase:         {true, true, false, true},
	// Open read-write database sessions.
	authz.OpenReadWriteDatabaseSession: {true, false, false, true},
	authz.TunnelDatabaseReadWrite:      {true, false, false, true},
	// Take backups, restore, and approve core updates.
	authz.CreateBackup:      {true, true, false, true},
	authz.RestoreBackup:     {true, true, false, true},
	authz.ApproveCoreUpdate: {true, true, false, true},
	// Create and edit client groups; manage alert rules.
	authz.CreateClientGroup: {true, true, false, true},
	authz.UpdateClientGroup: {true, true, false, true},
	authz.ManageAlertRules:  {true, true, false, true},
	// Delete sites.
	authz.DeleteSite: {true, false, false, true},
	// Delete a site with its backups: deleting backups is Admin-only.
	authz.DeleteSiteBackups: {true, false, false, true},
	// Delete or offboard client groups.
	authz.DeleteClientGroup:   {true, false, false, true},
	authz.OffboardClientGroup: {true, false, false, true},
	// Manage users, roles, and other users' API tokens; view the audit log.
	authz.ViewUsers:           {true, false, false, false},
	authz.CreateUser:          {true, false, false, true},
	authz.UpdateUser:          {true, false, false, true},
	authz.DeleteUser:          {true, false, false, true},
	authz.ManageOthersTokens:  {true, false, false, true},
	authz.ViewOthersSSHKeys:   {true, false, false, false},
	authz.ManageOthersSSHKeys: {true, false, false, true},
	authz.ViewAuditLog:        {true, false, false, false},
	// Reset other users' second factors.
	authz.ResetUserTwoFactor: {true, false, false, true},
	// Change platform settings.
	authz.ManagePlatformSettings: {true, false, false, true},
	// Manage their own profile (display name), password, API tokens, and SSH keys.
	authz.ViewSelf:          {true, true, true, false},
	authz.Logout:            {true, true, true, false},
	authz.UpdateOwnProfile:  {true, true, true, true},
	authz.ChangeOwnPassword: {true, true, true, true},
	authz.ViewOwnTokens:     {true, true, true, false},
	authz.CreateOwnToken:    {true, true, true, true},
	authz.RevokeOwnToken:    {true, true, true, true},
	authz.ViewOwnSSHKeys:    {true, true, true, false},
	authz.AddOwnSSHKey:      {true, true, true, true},
	authz.DeleteOwnSSHKey:   {true, true, true, true},
	// Replace their own recovery codes.
	authz.RegenerateRecoveryCodes: {true, true, true, true},
	// Register, rename, and remove their own security keys and passkeys.
	authz.RegisterWebAuthnCredential: {true, true, true, true},
	authz.RenameWebAuthnCredential:   {true, true, true, true},
	authz.RemoveWebAuthnCredential:   {true, true, true, true},
	// Enroll and remove their own authenticator app.
	authz.EnrollTOTP: {true, true, true, true},
	authz.RemoveTOTP: {true, true, true, true},
	// Reauthenticate for sensitive actions.
	authz.Reauthenticate:        {true, true, true, true},
	authz.StartReauthentication: {true, true, true, true},
}

// sensitiveDesign restates the plan's table of sensitive actions
// (docs/dashboard-two-factor-plan.md, design doc §6.2): the actions a
// session takes only soon after its user last proved who they are.
var sensitiveDesign = map[authz.Action]bool{
	// Add, remove, or reset second factors; regenerate recovery codes.
	authz.RegenerateRecoveryCodes:    true,
	authz.EnrollTOTP:                 true,
	authz.RemoveTOTP:                 true,
	authz.RegisterWebAuthnCredential: true,
	authz.RenameWebAuthnCredential:   true,
	authz.RemoveWebAuthnCredential:   true,
	authz.ResetUserTwoFactor:         true,
	// Create API tokens; add SSH keys.
	authz.CreateOwnToken: true,
	authz.AddOwnSSHKey:   true,
	// Create, update (including roles), and delete users; manage others'
	// tokens and SSH keys.
	authz.CreateUser:          true,
	authz.UpdateUser:          true,
	authz.DeleteUser:          true,
	authz.ManageOthersTokens:  true,
	authz.ManageOthersSSHKeys: true,
	// Open read-write database sessions.
	authz.OpenReadWriteDatabaseSession: true,
	// Delete sites, delete client groups, offboard client groups.
	authz.DeleteSite:          true,
	authz.DeleteSiteBackups:   true,
	authz.DeleteClientGroup:   true,
	authz.OffboardClientGroup: true,
}

// sshKeyActions are the actions an SSH key authenticates for; it can't
// take any other.
var sshKeyActions = map[authz.Action]bool{
	authz.ConnectSFTP: true, authz.TunnelDatabase: true, authz.TunnelDatabaseReadWrite: true,
}

// sessionOnlyActions are the actions API tokens can't take whatever their
// scope, because a token that could take them could outlive its revocation.
var sessionOnlyActions = map[authz.Action]bool{
	authz.CreateOwnToken:             true,
	authz.RegenerateRecoveryCodes:    true,
	authz.RegisterWebAuthnCredential: true,
	authz.RenameWebAuthnCredential:   true,
	authz.RemoveWebAuthnCredential:   true,
	authz.EnrollTOTP:                 true,
	authz.RemoveTOTP:                 true,
	authz.Reauthenticate:             true,
	authz.StartReauthentication:      true,
	authz.ResetUserTwoFactor:         true,
}

func TestDesignTableCoversEveryAction(t *testing.T) {
	actions := authz.Actions()
	if len(actions) != len(designTable) {
		t.Errorf("package defines %d actions, the test table has %d", len(actions), len(designTable))
	}
	for _, a := range actions {
		if _, ok := designTable[a]; !ok {
			t.Errorf("action %s is missing from the test table", a)
		}
		if a.Sensitive() != sensitiveDesign[a] {
			t.Errorf("%s: Sensitive() = %v, want %v", a, a.Sensitive(), sensitiveDesign[a])
		}
	}
	for a := range sensitiveDesign {
		if !a.Known() {
			t.Errorf("sensitive action %s isn't defined", a)
		}
	}
	// One-click wp-admin login is frequent and scoped to one site (§6.2).
	if authz.AdminLogin.Sensitive() {
		t.Error("AdminLogin must not be sensitive")
	}
}

// TestCheckReauthentication covers every action from fresh and stale
// sessions, token and SSH key callers, and the setting on and off.
func TestCheckReauthentication(t *testing.T) {
	const maxAge = 10 * time.Minute
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	on := auth.TwoFactorPolicy{Reauthentication: true, ReauthenticationMaxAge: maxAge}
	off := auth.TwoFactorPolicy{Reauthentication: false, ReauthenticationMaxAge: maxAge}
	callers := []struct {
		name    string
		p       auth.Principal
		session bool
	}{
		{"fresh session", auth.Principal{Method: auth.MethodSession, VerifiedAt: now.Add(-time.Minute)}, true},
		{"session at the max age", auth.Principal{Method: auth.MethodSession, VerifiedAt: now.Add(-maxAge)}, true},
		{"stale session", auth.Principal{Method: auth.MethodSession, VerifiedAt: now.Add(-maxAge - time.Second)}, true},
		{"session verified long ago", auth.Principal{Method: auth.MethodSession, VerifiedAt: now.Add(-11 * time.Minute)}, true},
		{"full token", auth.Principal{Method: auth.MethodToken, Scope: store.ScopeFull}, false},
		{"read-only token", auth.Principal{Method: auth.MethodToken, Scope: store.ScopeReadOnly}, false},
		{"ssh key", auth.Principal{Method: auth.MethodSSHKey}, false},
	}
	for action := range designTable {
		for _, c := range callers {
			for _, policy := range []struct {
				name string
				p    auth.TwoFactorPolicy
			}{{"on", on}, {"off", off}, {"zero", auth.TwoFactorPolicy{}}} {
				t.Run(string(action)+"/"+c.name+"/"+policy.name, func(t *testing.T) {
					p := c.p
					p.UserID, p.Role = id.New(), store.RoleAdmin
					stale := c.session && now.Sub(p.VerifiedAt) > maxAge
					want := policy.p.Reauthentication && sensitiveDesign[action] && stale
					err := authz.CheckReauthentication(p, action, policy.p, now)
					if got := err != nil; got != want {
						t.Fatalf("CheckReauthentication = %v, want refused = %v", err, want)
					}
					if err != nil && (!errors.Is(err, authz.ErrReauthenticationRequired) || !errors.Is(err, authz.ErrDenied)) {
						t.Errorf("refusal %v doesn't wrap ErrReauthenticationRequired and ErrDenied", err)
					}
				})
			}
		}
	}
}

func TestCheckEveryActionRoleAndCredential(t *testing.T) {
	creds := []struct {
		name   string
		method auth.Method
		scope  store.TokenScope
	}{
		{"session", auth.MethodSession, ""},
		{"full token", auth.MethodToken, store.ScopeFull},
		{"read-only token", auth.MethodToken, store.ScopeReadOnly},
		{"ssh key", auth.MethodSSHKey, ""},
	}
	for action, row := range designTable {
		for _, role := range store.Roles {
			for _, c := range creds {
				t.Run(string(action)+"/"+string(role)+"/"+c.name, func(t *testing.T) {
					allowed := map[store.Role]bool{store.RoleAdmin: row.admin, store.RoleOperator: row.operator, store.RoleViewer: row.viewer}[role]
					readOnlyWrite := c.scope == store.ScopeReadOnly && row.write
					sshElsewhere := c.method == auth.MethodSSHKey && !sshKeyActions[action]
					tokenSessionOnly := c.method == auth.MethodToken && sessionOnlyActions[action]
					want := allowed && !readOnlyWrite && !sshElsewhere && !tokenSessionOnly
					p := auth.Principal{UserID: id.New(), Role: role, Method: c.method, Scope: c.scope}
					if c.method == auth.MethodToken {
						p.TokenID = id.New()
					}
					err := authz.Check(p, action, authz.Site(id.New()))
					if got := err == nil; got != want {
						t.Fatalf("Check = %v, want allowed = %v", err, want)
					}
					if err != nil && !errors.Is(err, authz.ErrDenied) {
						t.Errorf("denial %v doesn't wrap ErrDenied", err)
					}
					if action.Writes() != row.write {
						t.Errorf("Writes() = %v, want %v", action.Writes(), row.write)
					}
				})
			}
		}
	}
}

func TestCheckFailsClosed(t *testing.T) {
	admin := auth.Principal{UserID: "u", Role: store.RoleAdmin, Method: auth.MethodSession}
	tests := []struct {
		name   string
		p      auth.Principal
		action authz.Action
	}{
		{"unknown action", admin, "site.explode"},
		{"empty action", admin, ""},
		{"unknown role", auth.Principal{Role: "root", Method: auth.MethodSession}, authz.ViewSites},
		{"no role", auth.Principal{Method: auth.MethodSession}, authz.ViewSelf},
		{"no method", auth.Principal{Role: store.RoleAdmin}, authz.ViewSites},
		{"token without scope", auth.Principal{Role: store.RoleAdmin, Method: auth.MethodToken}, authz.ViewSites},
		{"token with unknown scope", auth.Principal{Role: store.RoleAdmin, Method: auth.MethodToken, Scope: "site:abc"}, authz.ViewSites},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := authz.Check(tt.p, tt.action, authz.Platform()); !errors.Is(err, authz.ErrDenied) {
				t.Errorf("Check = %v, want ErrDenied", err)
			}
		})
	}
	if authz.Action("site.explode").Known() || !authz.Action("site.explode").Writes() {
		t.Error("unknown actions must be unknown and count as writes")
	}
}

func TestResolve(t *testing.T) {
	ctx := context.Background()
	// Without a database nothing is resolved, and nothing fails.
	if got, err := authz.NewResolver(nil).Resolve(ctx, authz.Site("x")); err != nil || got != authz.Site("x") {
		t.Errorf("Resolve without database = %+v, %v", got, err)
	}
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		group := &store.ClientGroup{Name: "Acme"}
		if err := store.NewClientGroups(db).Create(ctx, group); err != nil {
			t.Fatal(err)
		}
		site := id.New()
		now := time.Now()
		if _, err := db.ExecContext(ctx, `INSERT INTO sites
			(id, client_group_id, name, slug, status, preview_domain, php_version, wp_core_version, uid, created_at, updated_at)
			VALUES (?, ?, 'Site', 'site', 'running', 'site.preview.example', '8.4', '6.9.1', 20000, ?, ?)`,
			site, group.ID, now, now); err != nil {
			t.Fatal(err)
		}
		job := store.Job{Type: "site.update", TargetType: store.JobTargetSite, TargetID: site}
		if err := store.NewJobs(db).Enqueue(ctx, &job); err != nil {
			t.Fatal(err)
		}
		r := authz.NewResolver(db)
		for _, tt := range []struct {
			target authz.Target
			want   string
		}{
			{authz.Site(site), group.ID},
			{authz.Job(job.ID), group.ID},
			{authz.ClientGroup(group.ID), group.ID},
			{authz.Site(id.New()), ""},
			{authz.User(id.New()), ""},
			{authz.Platform(), ""},
		} {
			got, err := r.Resolve(ctx, tt.target)
			if err != nil || got.ClientGroupID != tt.want || got.Type != tt.target.Type || got.ID != tt.target.ID {
				t.Errorf("Resolve(%+v) = %+v, %v; want group %q", tt.target, got, err, tt.want)
			}
		}
	})
}

// enrollmentOnlyActions restates the actions a session that must enroll a
// second factor may take (design doc §6.2).
var enrollmentOnlyActions = map[authz.Action]bool{
	authz.ViewSelf:                   true,
	authz.Logout:                     true,
	authz.RegenerateRecoveryCodes:    true,
	authz.EnrollTOTP:                 true,
	authz.RemoveTOTP:                 true,
	authz.RegisterWebAuthnCredential: true,
	authz.Reauthenticate:             true,
	authz.StartReauthentication:      true,
}

// TestCheckRestrictsSessionsThatMustEnroll checks every action for every
// role and credential when the principal is marked EnrollmentRequired:
// sessions may take only the enrollment actions and are refused the rest
// with ErrEnrollmentRequired; tokens and SSH keys are decided as without
// the mark.
func TestCheckRestrictsSessionsThatMustEnroll(t *testing.T) {
	creds := []struct {
		name   string
		method auth.Method
		scope  store.TokenScope
	}{
		{"session", auth.MethodSession, ""},
		{"full token", auth.MethodToken, store.ScopeFull},
		{"ssh key", auth.MethodSSHKey, ""},
	}
	for action := range designTable {
		for _, role := range store.Roles {
			for _, c := range creds {
				t.Run(string(action)+"/"+string(role)+"/"+c.name, func(t *testing.T) {
					p := auth.Principal{UserID: id.New(), Role: role, Method: c.method, Scope: c.scope}
					unmarked := authz.Check(p, action, authz.Site(id.New()))
					p.EnrollmentRequired = true
					err := authz.Check(p, action, authz.Site(id.New()))
					if c.method != auth.MethodSession || enrollmentOnlyActions[action] {
						if (err == nil) != (unmarked == nil) {
							t.Fatalf("Check = %v, want as without the mark (%v)", err, unmarked)
						}
						if errors.Is(err, authz.ErrEnrollmentRequired) {
							t.Errorf("Check = %v, want no enrollment denial", err)
						}
						return
					}
					if !errors.Is(err, authz.ErrEnrollmentRequired) || !errors.Is(err, authz.ErrDenied) {
						t.Fatalf("Check = %v, want ErrEnrollmentRequired wrapping ErrDenied", err)
					}
				})
			}
		}
	}
}

// TestCheckRefusesResettingOwnSecondFactors checks that an Admin may reset
// another user's second factors but not their own, which would skip the
// rule that a user the policy covers keeps one.
func TestCheckRefusesResettingOwnSecondFactors(t *testing.T) {
	admin := auth.Principal{UserID: id.New(), Role: store.RoleAdmin, Method: auth.MethodSession}
	if err := authz.Check(admin, authz.ResetUserTwoFactor, authz.User(id.New())); err != nil {
		t.Errorf("resetting another user: %v", err)
	}
	if err := authz.Check(admin, authz.ResetUserTwoFactor, authz.User(admin.UserID)); !errors.Is(err, authz.ErrDenied) {
		t.Errorf("resetting oneself = %v, want ErrDenied", err)
	}
	// Other actions on oneself are decided by the role alone.
	if err := authz.Check(admin, authz.UpdateUser, authz.User(admin.UserID)); err != nil {
		t.Errorf("updating oneself: %v", err)
	}
}
