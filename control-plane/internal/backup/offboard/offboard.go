// Package offboard deletes a client group's backup repository from the
// dashboard (#139), so a client can be offboarded without a shell in the
// control plane container (design doc §4.6, "Delete or offboard client
// groups"; §5.3, "Repository Layout").
//
// # Job
//
// DeleteJobType deletes one group's repository; its target is the client
// group. Its steps are:
//
//  1. delete_repository: repository.Manager.Delete, which removes the
//     repository's files or S3 objects, then, in one transaction, the
//     records of the backups it held, its password, its maintenance
//     schedules, and the group's link to it. A group without a repository
//     has nothing to delete, so running the step again, or after a
//     concurrent `pressfleet repository delete`, succeeds without doing
//     anything. A group that has sites, or an export from its backups in
//     flight, fails the job for good before anything is removed.
//  2. delete_client_group: only when the job's payload asks for it
//     (DELETE /api/v1/client-groups/{id}?delete_backup_repository=true),
//     deletes the group itself, with the same checks as the API's delete.
//     A group that is already gone is done.
//
// Neither step creates anything, so a job that fails for good has nothing
// to roll back; the repository may already be gone when the second step
// fails (a site was created in the group meanwhile), which the job's error
// says.
//
// The job shares the repository's maintenance lock key
// (repomaint.LockKey), so it waits for a running prune or check instead of
// deleting the repository under it.
package offboard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup/repomaint"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/repository"
	"github.com/rchouinard/pressfleet/control-plane/internal/export"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// DeleteJobType is the type of the jobs that delete a client group's
// backup repository.
const DeleteJobType = "backup.repository_delete"

// Step names. Keep them stable: a queued job resumes by step name.
const (
	stepDeleteRepository = "delete_repository"
	stepDeleteGroup      = "delete_client_group"
)

// targetType is the jobs' target type; the target is the client group.
const targetType = "client_group"

// Sentinel errors callers branch on. Their messages, with Enqueue's
// wrapping, are fit to show the user (the API's Problem details).
var (
	// ErrClientGroupNotFound means the client group doesn't exist.
	ErrClientGroupNotFound = errors.New("client group not found")
	// ErrGroupHasSites means the group still has sites, whose backups the
	// repository holds.
	ErrGroupHasSites = errors.New("the client group still has sites")
	// ErrNoRepository means the group has no backup repository to delete.
	ErrNoRepository = errors.New("the client group has no backup repository")
	// ErrExportRunning means an export from the group's backups is queued
	// or running; it reads the repository.
	ErrExportRunning = errors.New("an export from the client group's backups is in progress")
	// ErrGroupHasRepository means the group got a backup repository again
	// before it could be deleted.
	ErrGroupHasRepository = errors.New("the client group has a backup repository")
)

// Repositories deletes backup repositories (*repository.Manager).
type Repositories interface {
	// Delete deletes the group's repository; a group without one is not an
	// error. It returns repository.ErrGroupHasSites while the group has
	// sites.
	Delete(ctx context.Context, clientGroupID string) error
}

// Options configure a Service.
type Options struct {
	// DB is the state database. Required.
	DB *store.DB
	// Repositories deletes the repositories. Required.
	Repositories Repositories
	// Logger defaults to discarding output.
	Logger *slog.Logger
}

// Service queues and runs repository deletions.
type Service struct {
	db    *store.DB
	repos Repositories
	log   *slog.Logger
}

// New returns a Service.
func New(opts Options) (*Service, error) {
	if opts.DB == nil || opts.Repositories == nil {
		return nil, errors.New("offboard: DB and Repositories are required")
	}
	s := &Service{db: opts.DB, repos: opts.Repositories, log: opts.Logger}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	return s, nil
}

// Handler returns the job handler, to register on the job pool.
func (s *Service) Handler() jobs.Handler {
	return jobs.Handler{Type: DeleteJobType, Steps: []jobs.Step{
		{Name: stepDeleteRepository, Do: s.deleteRepository},
		{Name: stepDeleteGroup, Do: s.deleteGroup},
	}}
}

// payload is the job's payload.
type payload struct {
	// DeleteClientGroup deletes the group once its repository is gone.
	DeleteClientGroup bool `json:"delete_client_group,omitempty"`
}

// Enqueue queues the deletion of the group's backup repository, and of the
// group itself afterwards with deleteGroup. It refuses, with the sentinel
// errors, a group that doesn't exist, has sites, has no repository, or has
// an export from its backups in flight.
func (s *Service) Enqueue(ctx context.Context, clientGroupID string, deleteGroup bool, createdByUserID string) (store.Job, error) {
	var job store.Job
	err := s.db.InTx(ctx, func(q store.Querier) error {
		g, err := store.NewClientGroups(q).Get(ctx, clientGroupID)
		if errors.Is(err, store.ErrNotFound) {
			return ErrClientGroupNotFound
		}
		if err != nil {
			return err
		}
		if g.SiteCount > 0 {
			return fmt.Errorf("%w (%d); delete them first", ErrGroupHasSites, g.SiteCount)
		}
		if g.BackupRepository == "" {
			return ErrNoRepository
		}
		if err := checkNoExports(ctx, q, clientGroupID); err != nil {
			return err
		}
		job, err = jobs.Enqueue(ctx, q, jobs.Spec{
			Type: DeleteJobType, TargetType: targetType, TargetID: clientGroupID,
			Payload: payload{DeleteClientGroup: deleteGroup}, LockKey: repomaint.LockKey(clientGroupID),
			CreatedByUserID: createdByUserID,
		})
		return err
	})
	switch {
	case errors.Is(err, ErrClientGroupNotFound), errors.Is(err, ErrGroupHasSites), errors.Is(err, ErrNoRepository),
		errors.Is(err, ErrExportRunning):
		return store.Job{}, err
	case err != nil:
		return store.Job{}, fmt.Errorf("queue deletion of the backup repository of client group %s: %w", clientGroupID, err)
	}
	s.log.InfoContext(ctx, "backup repository deletion queued", "client_group_id", clientGroupID,
		"delete_client_group", deleteGroup, "job_id", job.ID)
	return job, nil
}

// checkNoExports refuses while an export from one of the group's backups
// is queued or running: deleting the repository would fail it midway.
// Exports of a deleted site's backups target its client group; the group's
// live sites, which would be the targets of the others, must be gone.
func checkNoExports(ctx context.Context, q store.Querier, clientGroupID string) error {
	page, err := store.NewJobs(q).List(ctx, store.JobFilter{
		Types: []string{export.BackupJobType}, TargetType: targetType, TargetID: clientGroupID,
		States: []store.JobState{store.JobQueued, store.JobRunning, store.JobRollingBack}, Limit: 1,
	})
	if err != nil {
		return err
	}
	if len(page.Jobs) > 0 {
		return fmt.Errorf("%w (job %s); try again when it has finished", ErrExportRunning, page.Jobs[0].ID)
	}
	return nil
}

func (s *Service) deleteRepository(ctx context.Context, r *jobs.Run) error {
	groupID := r.Job().TargetID
	g, err := store.NewClientGroups(s.db).Get(ctx, groupID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// A group can't be deleted while it has a repository, so there is
		// none left to delete.
		r.Logf("The client group no longer exists; there is no backup repository to delete.")
		return nil
	case err != nil:
		return err
	case g.BackupRepository == "":
		r.Logf("The client group has no backup repository; nothing to delete.")
		return nil
	case g.SiteCount > 0:
		return jobs.Permanent(fmt.Errorf("%w (%d); delete them first", ErrGroupHasSites, g.SiteCount))
	}
	if err := checkNoExports(ctx, s.db, groupID); err != nil {
		if errors.Is(err, ErrExportRunning) {
			return jobs.Permanent(err)
		}
		return err
	}
	err = s.repos.Delete(ctx, groupID)
	if errors.Is(err, repository.ErrGroupHasSites) {
		return jobs.Permanent(err)
	}
	if err != nil {
		return err
	}
	s.log.InfoContext(ctx, "backup repository deleted from the dashboard", "client_group_id", groupID, "job_id", r.Job().ID)
	r.Logf("Deleted the backup repository %s, every backup in it, and its password.", g.BackupRepository)
	return nil
}

func (s *Service) deleteGroup(ctx context.Context, r *jobs.Run) error {
	var p payload
	if err := r.Payload(&p); err != nil {
		return jobs.Permanent(err)
	}
	if !p.DeleteClientGroup {
		return nil
	}
	groupID := r.Job().TargetID
	var name string
	err := s.db.InTx(ctx, func(q store.Querier) error {
		groups := store.NewClientGroups(q)
		g, err := groups.Get(ctx, groupID)
		if err != nil {
			return err
		}
		name = g.Name
		if g.SiteCount > 0 {
			return jobs.Permanent(fmt.Errorf("the backup repository was deleted, but the client group was not: %w (%d)", ErrGroupHasSites, g.SiteCount))
		}
		if g.BackupRepository != "" {
			return jobs.Permanent(fmt.Errorf("the client group was not deleted: %w again (a backup ran meanwhile)", ErrGroupHasRepository))
		}
		return groups.Delete(ctx, groupID)
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		r.Logf("The client group was already deleted.")
		return nil
	case errors.Is(err, store.ErrReference):
		// A site or repository was created concurrently, after the check.
		return jobs.Permanent(fmt.Errorf("the client group was not deleted: it got a site or a backup repository meanwhile: %w", err))
	case err != nil:
		return err
	}
	s.log.InfoContext(ctx, "client group deleted", "client_group_id", groupID, "job_id", r.Job().ID)
	r.Logf("Deleted the client group %s.", name)
	return nil
}
