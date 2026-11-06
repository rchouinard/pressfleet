// Package changes reports what the control plane changed since a time, for
// `pressfleet admin changes` (design doc §4.7, #248).
//
// `pressfleet upgrade --rollback` restores the state database the upgrade
// migrated, which undoes the records the new control plane wrote but not
// what it did to sites. Before it does, the host CLI asks the running
// control plane what changed since the upgrade started. The CLI never reads
// the state database itself, so the answer comes from here, through the
// admin interface.
//
// The changes come from two places:
//
//   - Jobs: everything that changes a site runs as a job (design doc §5),
//     whoever started it, so the jobs are the record of what was done to
//     sites, including what the rollouts did on their own. A job is listed
//     from the time it was queued, in whatever state it is in.
//   - The audit log (§4.6): changes to records that run no job, such as
//     users, alert rules, client groups, and settings. Only successful
//     events are listed, and only of actions that change something. A
//     request that queued a job is listed as its job.
//
// Each change falls in one of three groups, which say what restoring the
// state database does about it:
//
//   - [Blocking]: the restore would leave resources orphaned, a site
//     recreated empty, or a site on an older WordPress core than its
//     database. Sites created, imported, cloned (staging sites), or
//     deleted; core updates; go-lives, whose URL rewrite stays in the
//     site's database while the restored record has the old domains; and
//     client groups deleted or offboarded.
//   - [Converged]: the previous release's reconcile moves the sites back
//     to match the restored records. Template and image updates, and
//     settings changes.
//   - [Lost]: the records go with the restore, but what they describe
//     stays: backup snapshots in the repositories, content copied to sites
//     by staging refreshes and pushes, restores, and code deploys. Users,
//     alert rules, and the like are simply gone.
//
// A job that failed rolled back what it created (§5), so failed jobs of
// the kinds whose steps are all undone (site creation, import, cloning,
// and core updates) aren't listed. A site deletion can't be undone, so a
// failed one is.
package changes

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/audit"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/offboard"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/repomaint"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/sitebackup"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/statebackup"
	"github.com/rchouinard/pressfleet/control-plane/internal/dbsession"
	"github.com/rchouinard/pressfleet/control-plane/internal/deploy"
	"github.com/rchouinard/pressfleet/control-plane/internal/export"
	"github.com/rchouinard/pressfleet/control-plane/internal/pluginupdates"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitecron"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/uploads"
)

// Group says what restoring the state database does about a change.
type Group string

// Groups.
const (
	// Blocking changes leave sites broken or resources orphaned when the
	// state database is restored.
	Blocking Group = "blocking"
	// Converged changes are undone by the previous release's reconcile.
	Converged Group = "converged"
	// Lost changes lose their records; what they did stays.
	Lost Group = "lost"
	// ignore marks jobs and actions that change nothing worth reporting:
	// housekeeping, reads, sign-ins, and those reported from elsewhere.
	ignore Group = ""
)

// Report is the answer of `pressfleet admin changes`: the changes since a
// time, oldest first in each group.
type Report struct {
	Since     time.Time `json:"since"`
	Blocking  []Change  `json:"blocking"`
	Converged []Change  `json:"converged"`
	Lost      []Change  `json:"lost"`
}

// Change is one job or audit event.
type Change struct {
	// Kind is the job's type (e.g. "site.create") or the audit event's
	// action (e.g. "user.create").
	Kind string `json:"kind"`
	// At is when the job was queued or the event occurred.
	At time.Time `json:"at"`
	// JobID and JobState are the job's, for a change from a job.
	JobID    string         `json:"job_id,omitempty"`
	JobState store.JobState `json:"job_state,omitempty"`
	// AuditEventID is the event's, for a change from the audit log.
	AuditEventID string `json:"audit_event_id,omitempty"`
	// Site is the site changed, if any.
	Site *Site `json:"site,omitempty"`
	// ClientGroup is the client group changed, or the site's.
	ClientGroup *ClientGroup `json:"client_group,omitempty"`
	// TargetType and TargetID name any other target, such as a user or an
	// alert rule.
	TargetType string `json:"target_type,omitempty"`
	TargetID   string `json:"target_id,omitempty"`
}

// Site names a site. Slug is empty when the site's record is gone and its
// deletion didn't record it.
type Site struct {
	ID   string `json:"id"`
	Slug string `json:"slug,omitempty"`
	// Staging is true for a staging site (§5.11).
	Staging bool `json:"staging,omitempty"`
}

// ClientGroup names a client group. Name is empty when the group's record
// is gone.
type ClientGroup struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// jobGroups classifies job types. A type missing here is Lost: its record
// goes with the restore, and the test that lists every job type keeps the
// table complete.
var jobGroups = map[string]Group{
	sites.CreateJobType:     Blocking,
	sites.ImportJobType:     Blocking,
	sites.CloneJobType:      Blocking,
	sites.DeleteJobType:     Blocking,
	sites.CoreUpdateJobType: Blocking,
	// The URL rewrite stays in the site's database, while the restored
	// record and the previous reconcile route the old domains.
	sites.GoLiveJobType: Blocking,

	sites.TemplateUpdateJobType: Converged,
	sites.ImageUpdateJobType:    Converged,
	sites.UpdateJobType:         Converged,
	sites.StartJobType:          Converged,
	sites.StopJobType:           Converged,
	sites.MaintenanceJobType:    Converged,
	sites.AliasesJobType:        Converged,

	sites.RefreshJobType: Lost,
	sites.PushJobType:    Lost,
	sites.RestoreJobType: Lost,
	sites.RecoverJobType: Lost,
	sitebackup.JobType:   Lost,
	deploy.JobType:       Lost,
	export.JobType:       Lost,
	export.SiteJobType:   Lost,
	export.BackupJobType: Lost,

	// Offboarding is reported from the audit log (client_group.offboard),
	// along with the deletions of groups without a repository, which run
	// no job.
	offboard.DeleteJobType: ignore,
	// The reconcile and rollout passes queue the jobs that change sites,
	// which are listed themselves.
	sites.ReconcileJobType: ignore,
	sites.RolloutJobType:   ignore,
	// Housekeeping, which the previous release runs again.
	sites.PurgeCacheJobType: ignore,
	sitecron.JobType:        ignore,
	pluginupdates.JobType:   ignore,
	audit.CleanupJobType:    ignore,
	uploads.CleanupJobType:  ignore,
	export.CleanupJobType:   ignore,
	dbsession.SweepJobType:  ignore,
	repomaint.PruneJobType:  ignore,
	repomaint.CheckJobType:  ignore,
	statebackup.JobType:     ignore,
}

// undoneOnFailure are the job types whose failure rolls back everything
// they did (§5), so a failed one changed nothing.
var undoneOnFailure = map[string]bool{
	sites.CreateJobType:     true,
	sites.ImportJobType:     true,
	sites.CloneJobType:      true,
	sites.CoreUpdateJobType: true,
}

// actionGroups classifies the audit actions that change something. Actions
// that don't write (authz.Action.Writes) are ignored; a writing action
// missing here is Lost, and a test keeps the table complete.
var actionGroups = map[string]Group{
	string(authz.DeleteClientGroup):   Blocking,
	string(authz.OffboardClientGroup): Blocking,

	// Settings. Some requests under these actions run a job, which is
	// listed instead (see actionJobs).
	string(authz.UpdateSite):             Converged,
	string(authz.ApproveCoreUpdate):      Converged,
	string(authz.ManagePlatformSettings): Converged,

	string(authz.CreateClientGroup):          Lost,
	string(authz.UpdateClientGroup):          Lost,
	string(authz.ManageAlertRules):           Lost,
	string(authz.CreateUser):                 Lost,
	string(authz.UpdateUser):                 Lost,
	string(authz.DeleteUser):                 Lost,
	string(authz.ManageOthersTokens):         Lost,
	string(authz.ManageOthersSSHKeys):        Lost,
	string(authz.ResetUserTwoFactor):         Lost,
	string(authz.UpdateOwnProfile):           Lost,
	string(authz.ChangeOwnPassword):          Lost,
	string(authz.CreateOwnToken):             Lost,
	string(authz.RevokeOwnToken):             Lost,
	string(authz.AddOwnSSHKey):               Lost,
	string(authz.DeleteOwnSSHKey):            Lost,
	string(authz.RegenerateRecoveryCodes):    Lost,
	string(authz.RegisterWebAuthnCredential): Lost,
	string(authz.RenameWebAuthnCredential):   Lost,
	string(authz.RemoveWebAuthnCredential):   Lost,
	string(authz.EnrollTOTP):                 Lost,
	string(authz.RemoveTOTP):                 Lost,

	// These run a job, listed from the jobs.
	string(authz.CreateSite):         ignore,
	string(authz.ImportSite):         ignore,
	string(authz.StartSite):          ignore,
	string(authz.StopSite):           ignore,
	string(authz.SetSiteMaintenance): ignore,
	string(authz.ManageSiteDomains):  ignore,
	string(authz.PurgeSiteCache):     ignore,
	string(authz.CreateStagingSite):  ignore,
	string(authz.RefreshStagingSite): ignore,
	string(authz.PushStagingSite):    ignore,
	string(authz.DeleteStagingSite):  ignore,
	string(authz.DeleteSite):         ignore,
	string(authz.DeleteSiteBackups):  ignore,
	string(authz.CreateBackup):       ignore,
	string(authz.RestoreBackup):      ignore,
	string(authz.DeployCode):         ignore,
	string(authz.ExportSite):         ignore,
	string(authz.ExportDatabase):     ignore,
	// Access, sign-ins, and uploads, which change no lasting record.
	string(authz.RetrieveSiteCredentials):      ignore,
	string(authz.AdminLogin):                   ignore,
	string(authz.ConnectSFTP):                  ignore,
	string(authz.DownloadDatabaseExport):       ignore,
	string(authz.OpenDatabaseSession):          ignore,
	string(authz.EndDatabaseSession):           ignore,
	string(authz.TunnelDatabase):               ignore,
	string(authz.OpenReadWriteDatabaseSession): ignore,
	string(authz.TunnelDatabaseReadWrite):      ignore,
	string(authz.UploadForImport):              ignore,
	string(authz.UploadForDeploy):              ignore,
	string(authz.Logout):                       ignore,
	string(authz.Reauthenticate):               ignore,
	string(authz.StartReauthentication):        ignore,
	"self.login":                               ignore,
}

// actionJobs are the job types that requests under an action of
// actionGroups may queue. Requests under one action may or may not run a
// job (site.update changes settings with a job, and sets the backup
// schedule without one), so the action alone can't tell whether the
// request is listed as its job.
var actionJobs = map[string][]string{
	string(authz.UpdateSite):        {sites.UpdateJobType},
	string(authz.ApproveCoreUpdate): {sites.CoreUpdateJobType},
}

// coveredByJob is how long before its audit event a job queued by the
// same request may have been queued. The event is written once the
// response is complete, so the job comes first.
const coveredByJob = time.Minute

// pageSize is the page size of the queries.
const pageSize = 200

// Since reports the changes since since, from the jobs queued and the
// audit events that occurred at or after it.
func Since(ctx context.Context, q store.Querier, since time.Time) (Report, error) {
	since = since.UTC()
	r := Report{Since: since, Blocking: []Change{}, Converged: []Change{}, Lost: []Change{}}
	jobs, err := listJobs(ctx, q, since)
	if err != nil {
		return Report{}, err
	}
	events, err := listEvents(ctx, q, since)
	if err != nil {
		return Report{}, err
	}
	n := names{q: q, sites: map[string]*Site{}, groups: map[string]*ClientGroup{}, siteGroups: map[string]string{}}
	// Deleted sites have no row; their deletion jobs name them.
	for _, j := range jobs {
		if s, ok := sites.DeletedSite(j); ok {
			n.sites[s.ID] = &Site{ID: s.ID, Slug: s.Slug, Staging: s.ParentSiteID != ""}
			n.siteGroups[s.ID] = s.ClientGroupID
		}
	}

	add := func(g Group, c Change) {
		switch g {
		case Blocking:
			r.Blocking = append(r.Blocking, c)
		case Converged:
			r.Converged = append(r.Converged, c)
		case Lost:
			r.Lost = append(r.Lost, c)
		}
	}
	for _, j := range jobs {
		g, known := jobGroups[j.Type]
		if !known {
			g = Lost
		}
		if g == ignore || (j.State == store.JobFailed && undoneOnFailure[j.Type]) {
			continue
		}
		c := Change{Kind: j.Type, At: j.CreatedAt, JobID: j.ID, JobState: j.State}
		if err := n.target(ctx, &c, j.TargetType, j.TargetID); err != nil {
			return Report{}, err
		}
		if j.Type == sites.CloneJobType && c.Site != nil {
			c.Site.Staging = true
		}
		add(g, c)
	}
	for _, e := range events {
		g, known := actionGroups[e.Action]
		if !known {
			if !authz.Action(e.Action).Writes() {
				continue
			}
			g = Lost
		}
		if g == ignore || ranJob(e, jobs) {
			continue
		}
		c := Change{Kind: e.Action, At: e.OccurredAt, AuditEventID: e.ID}
		if err := n.target(ctx, &c, e.TargetType, e.TargetID); err != nil {
			return Report{}, err
		}
		add(g, c)
	}
	for _, list := range [][]Change{r.Blocking, r.Converged, r.Lost} {
		slices.SortStableFunc(list, func(a, b Change) int { return a.At.Compare(b.At) })
	}
	return r, nil
}

// listJobs returns the jobs queued since since, newest first, leaving out
// the types that are ignored.
func listJobs(ctx context.Context, q store.Querier, since time.Time) ([]store.Job, error) {
	var ignored []string
	for t, g := range jobGroups {
		if g == ignore {
			ignored = append(ignored, t)
		}
	}
	slices.Sort(ignored)
	var out []store.Job
	f := store.JobFilter{CreatedSince: since, ExcludeTypes: ignored, Limit: pageSize}
	for {
		page, err := store.NewJobs(q).List(ctx, f)
		if err != nil {
			return nil, fmt.Errorf("listing changes: %w", err)
		}
		out = append(out, page.Jobs...)
		if page.NextCursor == "" {
			return out, nil
		}
		f.Before = page.NextCursor
	}
}

// listEvents returns the successful audit events since since, newest
// first.
func listEvents(ctx context.Context, q store.Querier, since time.Time) ([]store.AuditEvent, error) {
	var out []store.AuditEvent
	f := store.AuditFilter{Since: since, Limit: pageSize}
	for {
		page, err := store.NewAuditEvents(q).List(ctx, f)
		if err != nil {
			return nil, fmt.Errorf("listing changes: %w", err)
		}
		for _, e := range page.Events {
			if e.Result == store.AuditSuccess {
				out = append(out, e)
			}
		}
		if page.NextCursor == "" {
			return out, nil
		}
		f.Before = page.NextCursor
	}
}

// ranJob reports whether the request e records queued one of jobs, which
// is listed instead: a job of a type the action may queue (actionJobs), on
// the same target, queued by the same user shortly before the event.
func ranJob(e store.AuditEvent, jobs []store.Job) bool {
	types := actionJobs[e.Action]
	if len(types) == 0 || e.TargetID == "" {
		return false
	}
	return slices.ContainsFunc(jobs, func(j store.Job) bool {
		return slices.Contains(types, j.Type) && j.TargetType == e.TargetType && j.TargetID == e.TargetID &&
			j.CreatedByUserID == e.ActorUserID && !j.CreatedAt.After(e.OccurredAt) && e.OccurredAt.Sub(j.CreatedAt) <= coveredByJob
	})
}

// names looks up the sites and client groups that changes target, once
// each.
type names struct {
	q          store.Querier
	sites      map[string]*Site
	groups     map[string]*ClientGroup
	siteGroups map[string]string // site ID to client group ID
}

// target sets c's target from a job's or event's target.
func (n *names) target(ctx context.Context, c *Change, targetType, targetID string) error {
	switch {
	case targetID == "":
		return nil
	case targetType == store.JobTargetSite:
		s, err := n.site(ctx, targetID)
		if err != nil {
			return err
		}
		site := *s
		c.Site = &site
		if gid := n.siteGroups[targetID]; gid != "" {
			c.ClientGroup, err = n.group(ctx, gid)
		}
		return err
	case targetType == store.JobTargetClientGroup:
		var err error
		c.ClientGroup, err = n.group(ctx, targetID)
		return err
	default:
		c.TargetType, c.TargetID = targetType, targetID
		return nil
	}
}

func (n *names) site(ctx context.Context, siteID string) (*Site, error) {
	if s, ok := n.sites[siteID]; ok {
		return s, nil
	}
	s := &Site{ID: siteID}
	row, err := store.NewSites(n.q).Get(ctx, siteID)
	switch {
	case err == nil:
		s.Slug, s.Staging = row.Slug, row.ParentSiteID != ""
		n.siteGroups[siteID] = row.ClientGroupID
	case !errors.Is(err, store.ErrNotFound):
		return nil, fmt.Errorf("listing changes: %w", err)
	}
	n.sites[siteID] = s
	return s, nil
}

func (n *names) group(ctx context.Context, groupID string) (*ClientGroup, error) {
	if g, ok := n.groups[groupID]; ok {
		return g, nil
	}
	g := &ClientGroup{ID: groupID}
	row, err := store.NewClientGroups(n.q).Get(ctx, groupID)
	switch {
	case err == nil:
		g.Name = row.Name
	case !errors.Is(err, store.ErrNotFound):
		return nil, fmt.Errorf("listing changes: %w", err)
	}
	n.groups[groupID] = g
	return g, nil
}
