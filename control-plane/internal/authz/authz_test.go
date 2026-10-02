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
	authz.UploadForImport:        {true, true, false, true},
	authz.UploadForDeploy:        {true, true, false, true},
	authz.ExportDatabase:         {true, true, false, true},
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
}

// sshKeyActions are the actions an SSH key authenticates for; it can't
// take any other.
var sshKeyActions = map[authz.Action]bool{
	authz.ConnectSFTP: true, authz.TunnelDatabase: true, authz.TunnelDatabaseReadWrite: true,
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
					want := allowed && !readOnlyWrite && !sshElsewhere
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

func TestCheckNewToken(t *testing.T) {
	now := time.Now()
	soon, later := now.Add(time.Hour), now.Add(48*time.Hour)
	session := auth.Principal{Role: store.RoleViewer, Method: auth.MethodSession}
	full := auth.Principal{Role: store.RoleAdmin, Method: auth.MethodToken, Scope: store.ScopeFull}
	readOnly := auth.Principal{Role: store.RoleAdmin, Method: auth.MethodToken, Scope: store.ScopeReadOnly}
	expiring := auth.Principal{Role: store.RoleAdmin, Method: auth.MethodToken, Scope: store.ScopeFull, TokenExpiresAt: &soon}
	earlier := now.Add(30 * time.Minute)
	tests := []struct {
		name    string
		p       auth.Principal
		scope   store.TokenScope
		expires *time.Time
		ok      bool
	}{
		{"session, full, no expiry", session, store.ScopeFull, nil, true},
		{"full token mints full", full, store.ScopeFull, nil, true},
		{"full token mints read-only", full, store.ScopeReadOnly, &later, true},
		{"read-only token mints read-only", readOnly, store.ScopeReadOnly, nil, true},
		{"read-only token mints full", readOnly, store.ScopeFull, nil, false},
		{"token mints unknown scope", full, "admin", nil, false},
		{"expiring token mints non-expiring", expiring, store.ScopeFull, nil, false},
		{"expiring token mints longer-lived", expiring, store.ScopeReadOnly, &later, false},
		{"expiring token mints same expiry", expiring, store.ScopeFull, &soon, true},
		{"expiring token mints shorter-lived", expiring, store.ScopeFull, &earlier, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := authz.CheckNewToken(tt.p, tt.scope, tt.expires)
			if (err == nil) != tt.ok {
				t.Fatalf("CheckNewToken = %v, want ok = %v", err, tt.ok)
			}
			if err != nil && !errors.Is(err, authz.ErrDenied) {
				t.Errorf("error %v doesn't wrap ErrDenied", err)
			}
		})
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
		job := store.Job{Type: "site.update", TargetType: "site", TargetID: site}
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
