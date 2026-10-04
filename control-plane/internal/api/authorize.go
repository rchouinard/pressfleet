package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
)

// operationActions declares the authorization action of every protected
// operation, by operationId. NewHandler refuses to start if a protected
// operation in the OpenAPI document is missing here, and authorize refuses
// a handler that checks a different action than its operation declares, so
// the declaration, the check, and the audit log agree.
var operationActions = map[string]authz.Action{
	"logout":            authz.Logout,
	"getCurrentUser":    authz.ViewSelf,
	"updateCurrentUser": authz.UpdateOwnProfile,
	"changePassword":    authz.ChangeOwnPassword,
	"listApiTokens":     authz.ViewOwnTokens,
	"createApiToken":    authz.CreateOwnToken,
	"revokeApiToken":    authz.RevokeOwnToken,

	"listUsers":  authz.ViewUsers,
	"createUser": authz.CreateUser,
	"getUser":    authz.ViewUsers,
	"updateUser": authz.UpdateUser,
	"deleteUser": authz.DeleteUser,

	"listOwnSshKeys":   authz.ViewOwnSSHKeys,
	"addOwnSshKey":     authz.AddOwnSSHKey,
	"deleteOwnSshKey":  authz.DeleteOwnSSHKey,
	"listUserSshKeys":  authz.ViewOthersSSHKeys,
	"addUserSshKey":    authz.ManageOthersSSHKeys,
	"deleteUserSshKey": authz.ManageOthersSSHKeys,

	"listClientGroups":  authz.ViewClientGroups,
	"createClientGroup": authz.CreateClientGroup,
	"getClientGroup":    authz.ViewClientGroups,
	"updateClientGroup": authz.UpdateClientGroup,
	"deleteClientGroup": authz.DeleteClientGroup,
	// Deleting a group's backups is offboarding (design doc §4.6).
	"deleteClientGroupBackupRepository": authz.OffboardClientGroup,

	"listSites":                    authz.ViewSites,
	"createSite":                   authz.CreateSite,
	"importSite":                   authz.ImportSite,
	"getSite":                      authz.ViewSites,
	"updateSite":                   authz.UpdateSite,
	"deleteSite":                   authz.DeleteSite,
	"startSite":                    authz.StartSite,
	"stopSite":                     authz.StopSite,
	"purgeSiteCache":               authz.PurgeSiteCache,
	"retrieveSiteAdminCredentials": authz.RetrieveSiteCredentials,
	// Preview password protection is a site setting (design doc §4.6).
	"regenerateSitePreviewPassword": authz.UpdateSite,
	"setSiteMaintenance":            authz.SetSiteMaintenance,
	"goLiveSite":                    authz.ManageSiteDomains,
	"setSiteAliases":                authz.ManageSiteDomains,

	"listSiteBackups":       authz.ViewBackups,
	"createSiteBackup":      authz.CreateBackup,
	"getSiteBackupSchedule": authz.ViewBackups,
	// The backup schedule is a site setting.
	"setSiteBackupSchedule": authz.UpdateSite,
	"restoreSite":           authz.RestoreBackup,

	"deploySiteCode": authz.DeployCode,

	"createSiteAdminLogin":   authz.AdminLogin,
	"listSiteAdministrators": authz.AdminLogin,

	"exportSiteDatabase": authz.ExportDatabase,
	// Site exports (§5.7): starting one and downloading its files hand
	// out the site's data; listing and reading them show none.
	"exportSite":                 authz.ExportSite,
	"exportSiteBackup":           authz.ExportSite,
	"exportClientGroupBackup":    authz.ExportSite,
	"downloadExportFile":         authz.ExportSite,
	"listExports":                authz.ViewSites,
	"getExport":                  authz.ViewSites,
	"listClientGroupBackups":     authz.ViewBackups,
	"downloadSiteDatabaseExport": authz.DownloadDatabaseExport,
	// Listing sessions shows no credentials.
	"listSiteDatabaseSessions":  authz.ViewSites,
	"createSiteDatabaseSession": authz.OpenDatabaseSession,
	"endSiteDatabaseSession":    authz.EndDatabaseSession,
	"getSiteCore":               authz.ViewSites,
	"setSiteCorePolicy":         authz.ApproveCoreUpdate,
	"updateSiteCore":            authz.ApproveCoreUpdate,
	"getClientGroupCorePolicy":  authz.ViewClientGroups,
	"setClientGroupCorePolicy":  authz.ApproveCoreUpdate,
	"getCoreRollout":            authz.ViewSites,
	"resumeCoreRollout":         authz.ApproveCoreUpdate,
	// Resuming a rollout is the same decision for images and templates as
	// for core
	// releases.
	"getImageRollout":       authz.ViewSites,
	"getPlatform":           authz.ViewSites,
	"listPHPVersions":       authz.ViewSites,
	"resumeImageRollout":    authz.ApproveCoreUpdate,
	"getTemplateRollout":    authz.ViewSites,
	"resumeTemplateRollout": authz.ApproveCoreUpdate,

	"listJobs": authz.ViewJobs,
	"getJob":   authz.ViewJobs,

	// Runtime logs are monitoring data; a site's job logs are job data.
	"listSiteLogs":    authz.ViewMonitoring,
	"streamSiteLogs":  authz.ViewMonitoring,
	"listSiteJobLogs": authz.ViewJobs,

	// Metrics, like logs, are monitoring data.
	"getMetricCatalog":          authz.ViewMonitoring,
	"queryPlatformMetrics":      authz.ViewMonitoring,
	"getPlatformMetricsSummary": authz.ViewMonitoring,
	"getPlatformCapacity":       authz.ViewMonitoring,
	"querySiteMetrics":          authz.ViewMonitoring,
	"getSiteMetricsSummary":     authz.ViewMonitoring,

	// The doctor report is platform health, so monitoring data.
	"getPlatformDoctor": authz.ViewMonitoring,

	"listAuditEvents": authz.ViewAuditLog,

	// Alert rules are managed by Operators and Admins (design doc §4.6);
	// every role reads them and the alerts they raise.
	"listAlertRules":   authz.ViewMonitoring,
	"getAlertRule":     authz.ViewMonitoring,
	"createAlertRule":  authz.ManageAlertRules,
	"updateAlertRule":  authz.ManageAlertRules,
	"deleteAlertRule":  authz.ManageAlertRules,
	"enableAlertRule":  authz.ManageAlertRules,
	"disableAlertRule": authz.ManageAlertRules,
	"listAlerts":       authz.ViewMonitoring,
	"getAlert":         authz.ViewMonitoring,

	// Alert channels are platform settings (design doc §4.6): Admins
	// manage them, and every role reads them, without credentials.
	"listAlertChannels":   authz.ViewMonitoring,
	"getAlertChannel":     authz.ViewMonitoring,
	"createAlertChannel":  authz.ManagePlatformSettings,
	"updateAlertChannel":  authz.ManagePlatformSettings,
	"deleteAlertChannel":  authz.ManagePlatformSettings,
	"enableAlertChannel":  authz.ManagePlatformSettings,
	"disableAlertChannel": authz.ManagePlatformSettings,
	"testAlertChannel":    authz.ManagePlatformSettings,
	"listAlertDeliveries": authz.ViewMonitoring,

	// An upload needs the permission of the job it is for (#111): these
	// check UploadForDeploy instead for deployments (operationAltActions).
	// Requests for uploads that don't exist are checked as imports.
	"createUpload":      authz.UploadForImport,
	"getUpload":         authz.UploadForImport,
	"appendUploadChunk": authz.UploadForImport,
	"completeUpload":    authz.UploadForImport,
	"deleteUpload":      authz.UploadForImport,
}

// operationAltActions declares the other actions an operation's handler
// may check, for operations whose action depends on their target. The
// audit log records the action the handler checked; requests that fail
// before reaching the handler, such as invalid ones, are recorded with the
// operationActions one.
var operationAltActions = map[string][]authz.Action{
	// Read-write sessions need an Admin.
	"createSiteDatabaseSession": {authz.OpenReadWriteDatabaseSession},
	"createUpload":              {authz.UploadForDeploy},
	"getUpload":                 {authz.UploadForDeploy},
	"appendUploadChunk":         {authz.UploadForDeploy},
	"completeUpload":            {authz.UploadForDeploy},
	"deleteUpload":              {authz.UploadForDeploy},

	// Deleting a site's backups with it needs the permission to delete
	// backups.
	"deleteSite": {authz.DeleteSiteBackups},

	// Deleting the group's backups with it is offboarding.
	"deleteClientGroup": {authz.OffboardClientGroup},
}

// errActionMismatch is a programming error: a handler checked a different
// action than operationActions declares for its operation.
var errActionMismatch = errors.New("api: handler checked an action its operation doesn't declare")

// authorize is the single authorization check (design doc §4.6). Every
// protected handler calls it first, before reading or changing anything,
// and returns its error as is: ErrForbidden (403) when the caller may not
// take the action on the target. It returns the caller.
func (h *Handlers) authorize(ctx context.Context, action authz.Action, target authz.Target) (auth.Principal, error) {
	if op, ok := OperationFromContext(ctx); ok && !op.allows(action) {
		return auth.Principal{}, fmt.Errorf("%w: %s declares %q, handler checked %q", errActionMismatch, op.ID, op.Action, action)
	}
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok {
		return auth.Principal{}, ErrUnauthenticated
	}
	target, err := h.resolver.Resolve(ctx, target)
	if err != nil {
		return auth.Principal{}, err
	}
	err = authz.Check(p, action, target)
	noteCheck(ctx, action, target, err != nil)
	if err != nil {
		return auth.Principal{}, forbidden(err)
	}
	return p, nil
}

// deny refuses a request after authorize allowed it, for checks that depend
// on the request body (see authz.CheckNewToken). It is recorded in the
// audit log as a denial, like one from authorize.
func deny(ctx context.Context, err error) error {
	if rec := auditFromContext(ctx); rec != nil {
		rec.denied = true
	}
	return forbidden(err)
}

// self is the target of actions on the caller's own account.
func self(ctx context.Context) authz.Target {
	p, _ := auth.PrincipalFromContext(ctx)
	return authz.User(p.UserID)
}

// forbidden converts an authz denial into the API's 403 error. The authz
// message says which role or scope was missing, which is safe to show the
// caller.
func forbidden(err error) error {
	return fmt.Errorf("%w: %w", ErrForbidden, err)
}
