// Package authz decides what an authenticated caller may do (design doc
// §4.6). It answers "may this principal take this action on this target?"
// with one function, [Check], and keeps the action → role table from §4.6
// in one place, [rules].
//
// # Roles and scopes
//
// Each user has one platform-wide role: Admin, Operator, or Viewer. The
// §4.6 table is monotonic (everything a Viewer may do, an Operator may do,
// and everything an Operator may do, an Admin may do), so each action names
// the least role that may take it.
//
// An API token's effective permissions are its scope intersected with its
// owner's current role: a read-only token may only take read actions, and a
// full token may take everything its owner's role allows. Sessions have no
// scope. The role in the Principal is read from the database on every
// request (package auth), so demoting a user restricts their existing
// sessions and tokens at once.
//
// # Targets and client groups
//
// Every check names a target, and [Resolver.Resolve] fills in the client
// group the target belongs to (a site's group, a job's target's group). The
// MVP doesn't use it: roles are platform-wide. Client-group-scoped access
// (post-MVP) only needs a user↔group membership check in [Check], and
// future site-scoped API tokens plug in the same way (design doc §4.2).
package authz

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// ErrDenied means the caller's role or token scope doesn't allow the
// action. Denials match it with errors.Is; their message is only the
// reason, which is safe to show the caller.
var ErrDenied = errors.New("permission denied")

// deniedError is a denial with its reason.
type deniedError struct{ reason string }

func (e *deniedError) Error() string        { return e.reason }
func (e *deniedError) Is(target error) bool { return target == ErrDenied }

func denied(format string, args ...any) error {
	return &deniedError{reason: fmt.Sprintf(format, args...)}
}

// Action is something a caller may be allowed to do. Actions are also the
// action names recorded in the audit log, so they must stay stable.
type Action string

// Actions, grouped by the row of the design doc §4.6 table that governs
// them. Some have no API endpoint yet; they are listed so the table here is
// complete and the endpoints that arrive later only need to use them.
const (
	// View sites, client groups, jobs, backups, metrics, logs, and alerts.
	ViewSites        Action = "site.view"
	ViewClientGroups Action = "client_group.view"
	ViewJobs         Action = "job.view"
	ViewBackups      Action = "backup.view"
	ViewMonitoring   Action = "monitoring.view" // metrics, logs, and alerts

	// Create and import sites; change site settings (PHP version, core branch hold,
	// resources, preview password protection); go-live and domains;
	// start/stop; maintenance mode; cache purge.
	CreateSite              Action = "site.create"
	ImportSite              Action = "site.import"
	RetrieveSiteCredentials Action = "site.credentials.retrieve" // the one-time admin password of a new site
	UpdateSite              Action = "site.update"
	ManageSiteDomains       Action = "site.domains.manage"
	StartSite               Action = "site.start"
	StopSite                Action = "site.stop"
	SetSiteMaintenance      Action = "site.maintenance.set"
	PurgeSiteCache          Action = "site.cache.purge"

	// Deploy code; connect over SFTP; export the database; open read-only
	// database sessions (§5.8).
	ConnectSFTP            Action = "site.sftp.connect"
	DeployCode             Action = "site.code.deploy"
	ExportDatabase         Action = "site.database.export"
	DownloadDatabaseExport Action = "site.database.export.download"
	OpenDatabaseSession    Action = "site.database.session.create"
	EndDatabaseSession     Action = "site.database.session.end"
	// TunnelDatabase is forwarding a connection through the SFTP gateway
	// to the database of a site on which the user has an open read-only
	// database session. SSH keys take it; see sshActions.
	TunnelDatabase Action = "site.database.tunnel"

	// Open read-write database sessions (§5.8), and tunnel to the database
	// with one.
	OpenReadWriteDatabaseSession Action = "site.database.session.create_read_write"
	TunnelDatabaseReadWrite      Action = "site.database.tunnel_read_write"

	// Send a large file through the upload API (#111) for a site import or
	// a code deployment. Each needs the permission of the job it is for.
	UploadForImport Action = "upload.site_import"
	UploadForDeploy Action = "upload.site_deploy"

	// Take backups, restore, and approve core updates.
	CreateBackup      Action = "backup.create"
	RestoreBackup     Action = "backup.restore"
	ApproveCoreUpdate Action = "core_update.approve"

	// Create and edit client groups; manage alert rules.
	CreateClientGroup Action = "client_group.create"
	UpdateClientGroup Action = "client_group.update"
	ManageAlertRules  Action = "alert_rule.manage"

	// Delete sites.
	DeleteSite Action = "site.delete"

	// Delete or offboard client groups (export or delete the backup
	// repository and its key).
	DeleteClientGroup   Action = "client_group.delete"
	OffboardClientGroup Action = "client_group.offboard"

	// Manage users, roles, and other users' API tokens; view the audit log.
	// Other users' SSH keys belong here too.
	ViewUsers           Action = "user.view"
	CreateUser          Action = "user.create"
	UpdateUser          Action = "user.update" // including the role
	DeleteUser          Action = "user.delete"
	ManageOthersTokens  Action = "user.tokens.manage"
	ViewOthersSSHKeys   Action = "user.ssh_keys.view"
	ManageOthersSSHKeys Action = "user.ssh_keys.manage"
	ViewAuditLog        Action = "audit.view"

	// Change platform settings (DNS and S3 credentials, SMTP, alert
	// channels, backup location, retention).
	ManagePlatformSettings Action = "platform.settings.manage"

	// Manage their own profile, password, API tokens, and SSH keys. Viewing oneself
	// and logging out belong here too.
	ViewSelf          Action = "self.view"
	Logout            Action = "self.logout"
	UpdateOwnProfile  Action = "self.profile.update"
	ChangeOwnPassword Action = "self.password.change"
	ViewOwnTokens     Action = "self.tokens.view"
	CreateOwnToken    Action = "self.tokens.create" //nolint:gosec // G101: an action name, not a credential
	RevokeOwnToken    Action = "self.tokens.revoke" //nolint:gosec // G101: an action name, not a credential
	ViewOwnSSHKeys    Action = "self.ssh_keys.view"
	AddOwnSSHKey      Action = "self.ssh_keys.create"
	DeleteOwnSSHKey   Action = "self.ssh_keys.delete"
)

// sshActions are the only actions an SSH key (auth.MethodSSHKey) may take:
// it authenticates to the SFTP gateway, never to the API.
var sshActions = map[Action]bool{
	ConnectSFTP: true,
	// The gateway checks these for every tunneled database connection, so
	// losing the role ends access to open sessions too.
	TunnelDatabase:          true,
	TunnelDatabaseReadWrite: true,
}

// rule says who may take an action.
type rule struct {
	// min is the least role allowed.
	min store.Role
	// write marks actions that change state; read-only tokens can't take
	// them.
	write bool
}

// rules is the design doc §4.6 action → role table. It is the only place
// that decides which role may do what.
var rules = map[Action]rule{
	ViewSites:        {store.RoleViewer, false},
	ViewClientGroups: {store.RoleViewer, false},
	ViewJobs:         {store.RoleViewer, false},
	ViewBackups:      {store.RoleViewer, false},
	ViewMonitoring:   {store.RoleViewer, false},

	CreateSite:              {store.RoleOperator, true},
	RetrieveSiteCredentials: {store.RoleOperator, true}, // consumes the one-time credentials
	UpdateSite:              {store.RoleOperator, true},
	ManageSiteDomains:       {store.RoleOperator, true},
	StartSite:               {store.RoleOperator, true},
	StopSite:                {store.RoleOperator, true},
	SetSiteMaintenance:      {store.RoleOperator, true},
	PurgeSiteCache:          {store.RoleOperator, true},
	ImportSite:              {store.RoleOperator, true},

	DeployCode: {store.RoleOperator, true},

	// An upload needs the permission of the job it is for.
	UploadForImport: {store.RoleOperator, true}, // as ImportSite
	UploadForDeploy: {store.RoleOperator, true}, // as DeployCode

	ExportDatabase: {store.RoleOperator, true},
	// Reading a download changes nothing, but it hands out the site's
	// whole database, which a read-only token must not reach: it couldn't
	// start the export either.
	DownloadDatabaseExport: {store.RoleOperator, true},

	ConnectSFTP: {store.RoleOperator, true}, // SFTP sessions write files

	// A read-only database session still hands out the site's data, which
	// a read-only token must not reach, like an export's download.
	OpenDatabaseSession: {store.RoleOperator, true},
	EndDatabaseSession:  {store.RoleOperator, true},
	TunnelDatabase:      {store.RoleOperator, true},

	OpenReadWriteDatabaseSession: {store.RoleAdmin, true},
	TunnelDatabaseReadWrite:      {store.RoleAdmin, true},

	CreateBackup:      {store.RoleOperator, true},
	RestoreBackup:     {store.RoleOperator, true},
	ApproveCoreUpdate: {store.RoleOperator, true},

	CreateClientGroup: {store.RoleOperator, true},
	UpdateClientGroup: {store.RoleOperator, true},
	ManageAlertRules:  {store.RoleOperator, true},

	DeleteSite: {store.RoleAdmin, true},

	DeleteClientGroup:   {store.RoleAdmin, true},
	OffboardClientGroup: {store.RoleAdmin, true},

	ViewUsers:           {store.RoleAdmin, false},
	CreateUser:          {store.RoleAdmin, true},
	UpdateUser:          {store.RoleAdmin, true},
	DeleteUser:          {store.RoleAdmin, true},
	ManageOthersTokens:  {store.RoleAdmin, true},
	ViewOthersSSHKeys:   {store.RoleAdmin, false},
	ManageOthersSSHKeys: {store.RoleAdmin, true},
	ViewAuditLog:        {store.RoleAdmin, false},

	ManagePlatformSettings: {store.RoleAdmin, true},

	ViewSelf: {store.RoleViewer, false},
	// Logging out a token caller only clears the (absent) cookie, and a
	// session has no scope, so logging out is not a write for scope
	// purposes.
	Logout:            {store.RoleViewer, false},
	UpdateOwnProfile:  {store.RoleViewer, true},
	ChangeOwnPassword: {store.RoleViewer, true},
	ViewOwnTokens:     {store.RoleViewer, false},
	CreateOwnToken:    {store.RoleViewer, true},
	RevokeOwnToken:    {store.RoleViewer, true},
	// Any role may register keys; connecting still takes ConnectSFTP.
	ViewOwnSSHKeys:  {store.RoleViewer, false},
	AddOwnSSHKey:    {store.RoleViewer, true},
	DeleteOwnSSHKey: {store.RoleViewer, true},
}

// Actions returns every action, for tests and documentation.
func Actions() []Action {
	out := make([]Action, 0, len(rules))
	for a := range rules {
		out = append(out, a)
	}
	return out
}

// Known reports whether a is a defined action.
func (a Action) Known() bool {
	_, ok := rules[a]
	return ok
}

// Writes reports whether a changes state, so read-only tokens can't take
// it. Unknown actions count as writes.
func (a Action) Writes() bool {
	r, ok := rules[a]
	return !ok || r.write
}

// rank orders roles; unknown roles rank below every real one.
func rank(r store.Role) int {
	switch r {
	case store.RoleViewer:
		return 1
	case store.RoleOperator:
		return 2
	case store.RoleAdmin:
		return 3
	}
	return 0
}

// TargetType names the kind of thing an action applies to.
type TargetType string

// Target types. The empty type is the platform as a whole, for actions on
// collections (list sites, create a user) and settings.
const (
	TargetPlatform    TargetType = ""
	TargetSite        TargetType = "site"
	TargetClientGroup TargetType = "client_group"
	TargetJob         TargetType = "job"
	TargetUser        TargetType = "user"
	TargetAPIToken    TargetType = "api_token"
	TargetUpload      TargetType = "upload"
	TargetSSHKey      TargetType = "ssh_key"
	TargetAlertRule   TargetType = "alert_rule"
	TargetAlert       TargetType = "alert"
	// TargetAlertChannel is an alert channel (#76), a platform setting
	// that belongs to no client group.
	TargetAlertChannel TargetType = "alert_channel"
)

// Target is what an action applies to.
type Target struct {
	Type TargetType
	ID   string
	// ClientGroupID is the client group the target belongs to, filled in
	// by Resolver.Resolve. It is empty for targets outside any group
	// (users, the platform) and for targets that don't exist.
	ClientGroupID string
}

// Platform is the target for actions on collections and settings.
func Platform() Target { return Target{} }

// Site returns the target for a site.
func Site(id string) Target { return Target{Type: TargetSite, ID: id} }

// ClientGroup returns the target for a client group. An empty ID means the
// platform, e.g. for listing without a group filter.
func ClientGroup(id string) Target {
	if id == "" {
		return Platform()
	}
	return Target{Type: TargetClientGroup, ID: id}
}

// Job returns the target for a job.
func Job(id string) Target { return Target{Type: TargetJob, ID: id} }

// User returns the target for a user.
func User(id string) Target { return Target{Type: TargetUser, ID: id} }

// APIToken returns the target for an API token.
func APIToken(id string) Target { return Target{Type: TargetAPIToken, ID: id} }

// Upload returns the target for an upload (#111), which belongs to the
// client group of the import or deployment it is for.
func Upload(id string) Target { return Target{Type: TargetUpload, ID: id} }

// SSHKey returns the target for an SSH key.
func SSHKey(id string) Target { return Target{Type: TargetSSHKey, ID: id} }

// AlertRule returns the target for an alert rule, which belongs to the
// client group it is limited to, if any.
func AlertRule(id string) Target { return Target{Type: TargetAlertRule, ID: id} }

// Alert returns the target for an alert, which belongs to the client group
// of its site.
func Alert(id string) Target { return Target{Type: TargetAlert, ID: id} }

// AlertChannel returns the target for an alert channel.
func AlertChannel(id string) Target { return Target{Type: TargetAlertChannel, ID: id} }

// Check is the single authorization decision: it returns nil if p may take
// action on t, and an error wrapping ErrDenied otherwise. t should have been
// resolved (Resolver.Resolve) so that client-group scoping can be added
// here without touching callers.
//
// It fails closed: unknown actions, roles, authentication methods, and
// token scopes are denied.
func Check(p auth.Principal, action Action, t Target) error {
	r, ok := rules[action]
	if !ok {
		return denied("unknown action %q", action)
	}
	have := rank(p.Role)
	if have == 0 {
		return denied("unknown role %q", p.Role)
	}
	if have < rank(r.min) {
		return denied("the %s role doesn't allow %s", p.Role, action)
	}
	switch p.Method {
	case auth.MethodSession:
	case auth.MethodToken:
		switch p.Scope {
		case store.ScopeFull:
		case store.ScopeReadOnly:
			if r.write {
				return denied("a read-only API token doesn't allow %s", action)
			}
		default:
			return denied("unknown token scope %q", p.Scope)
		}
	case auth.MethodSSHKey:
		if !sshActions[action] {
			return denied("an SSH key doesn't allow %s", action)
		}
	default:
		return denied("unknown authentication method %q", p.Method)
	}
	// Client-group scoping (post-MVP) goes here, using t.ClientGroupID.
	_ = t
	return nil
}

// CheckNewToken refuses to let a caller authenticated by an API token mint
// a token that can do more than the caller's own: a broader scope, or a
// longer (or no) expiry. Sessions may create any token their role allows;
// the role still caps what the token can do.
func CheckNewToken(p auth.Principal, scope store.TokenScope, expiresAt *time.Time) error {
	if p.Method != auth.MethodToken {
		return nil
	}
	if scopeRank(scope) > scopeRank(p.Scope) {
		return denied("an API token with scope %s can't create a token with scope %s", p.Scope, scope)
	}
	if p.TokenExpiresAt != nil && (expiresAt == nil || expiresAt.After(*p.TokenExpiresAt)) {
		return denied("a token created with an API token can't expire later than that token (%s)",
			p.TokenExpiresAt.UTC().Format(time.RFC3339))
	}
	return nil
}

// scopeRank orders scopes by breadth; unknown scopes are broadest, so they
// are never considered within another scope.
func scopeRank(s store.TokenScope) int {
	switch s {
	case store.ScopeReadOnly:
		return 1
	case store.ScopeFull:
		return 2
	}
	return 3
}

// Resolver fills in the client group of a target.
type Resolver struct {
	q store.Querier
}

// NewResolver returns a Resolver that looks targets up with q. A nil q
// resolves nothing, for tests that run without a database.
func NewResolver(q store.Querier) Resolver {
	return Resolver{q: q}
}

// Resolve returns t with ClientGroupID filled in. A target that doesn't
// exist resolves to no group rather than an error, so the handler can
// report it as not found after the check.
func (r Resolver) Resolve(ctx context.Context, t Target) (Target, error) {
	if r.q == nil || t.ClientGroupID != "" {
		return t, nil
	}
	group, err := store.TargetClientGroup(ctx, r.q, string(t.Type), t.ID)
	if err != nil {
		return t, fmt.Errorf("resolving the client group of %s %s: %w", t.Type, t.ID, err)
	}
	t.ClientGroupID = group
	return t, nil
}
