package repository

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Repositories left by a state restore (#277).
//
// A state restore brings back the database as it was, but not the
// repositories, which live beside it. A group that got its first
// repository after the restored backup was taken has files and no record:
// its next backup stores a new password, which doesn't open them, and every
// backup of the group would fail with ErrWrongPassword. Nothing in the
// files says which password they were made with, and the one that did is
// gone with the record, so they can't be opened; they can only be kept.
//
// So the record says whether its password has ever opened the repository
// (Repository.PasswordVerifiedAt). Until it has, verify opens the
// repository before it is used, and a wrong password then means the files
// aren't this record's: they are set aside, never deleted, and a new
// repository is created with the stored password. Once the password has
// opened it, a wrong password is the operator's problem (a copied-in or
// damaged repository, docs/troubleshooting.md), and moves nothing. A record
// made by the call that hits the wrong password isn't a usable signal
// instead: it misses groups already stuck when this shipped, and a retry
// after a crash between creating the record and setting the files aside.

// AuditActionSetAside is the audit log action of setting a repository
// aside. The control plane does it on its own, so the event has no actor
// user or token; its details name the old and new locations.
const AuditActionSetAside = "backup_repository.set_aside"

// unopenablePrefix starts the name of a local repository set aside, in the
// backups directory next to the repositories directory, so nothing takes it
// for a group's repository.
const unopenablePrefix = "unopenable-"

// maxVerifyRounds bounds verify's retries when another caller changes the
// record under it. One round sets the repository aside and the next
// creates the new one; more mean something else keeps changing it.
const maxVerifyRounds = 3

// verify readies a repository whose password hasn't been seen to open it
// yet: it opens it, creates it if it doesn't exist, or sets it aside if the
// password doesn't open it and creates a new one. Then it marks the record.
// Any other failure, such as an unreachable S3 service, is returned and
// changes nothing. It returns the record and password to use, which a
// concurrent caller may have changed.
func (m *Manager) verify(ctx context.Context, rec Repository, password redact.Secret) (Repository, redact.Secret, error) {
	for range maxVerifyRounds {
		ok, err := m.ready(ctx, rec, password)
		switch {
		case ok:
		case !errors.Is(err, ErrWrongPassword):
			// Missing, or a failure initialize judges again under its lock,
			// returning it if it persists.
			if err := m.initialize(ctx, rec, password); err != nil {
				return Repository{}, "", err
			}
		default:
			next, err := m.setAside(ctx, rec, password)
			if err != nil {
				return Repository{}, "", err
			}
			if next.SecretRef != rec.SecretRef {
				if password, err = m.password(ctx, next); err != nil {
					return Repository{}, "", err
				}
			}
			rec = next
			if !rec.PasswordVerifiedAt.IsZero() {
				return rec, password, nil
			}
			continue
		}
		at := time.Now().UTC()
		if err := markVerified(ctx, m.db, rec, at); err != nil {
			return Repository{}, "", err
		}
		rec.PasswordVerifiedAt = at
		return rec, password, nil
	}
	return Repository{}, "", fmt.Errorf("the record of the repository at %s kept changing while it was being opened", rec.Location)
}

// setAside moves aside a repository that rec's password doesn't open, so
// that a new one can be created: a local repository is renamed to
// <data_dir>/backups/unopenable-<group>-<time>, and on S3, where a rename
// is a copy of every object, the objects stay where they are and the
// record moves to a new location beside them instead. Nothing is deleted.
// It runs under the repository's init lock and judges again there, since
// another caller may have set it aside already: it returns the record as it
// now is, changed or not, for verify to start over with.
func (m *Manager) setAside(ctx context.Context, rec Repository, password redact.Secret) (Repository, error) {
	unlock, err := initLocks.lock(ctx, rec.Location)
	if err != nil {
		return Repository{}, fmt.Errorf("waiting for the repository to be initialized: %w", err)
	}
	defer unlock()

	cur, err := getRecord(ctx, m.db, rec.ClientGroupID)
	if err != nil {
		return Repository{}, err
	}
	if cur.Location != rec.Location || cur.SecretRef != rec.SecretRef || !cur.PasswordVerifiedAt.IsZero() {
		return cur, nil
	}
	if err := m.probe(ctx, rec, password); !errors.Is(err, ErrWrongPassword) {
		return cur, nil
	}
	switch rec.Backend {
	case BackendLocal:
		return m.setAsideLocal(ctx, rec)
	case BackendS3:
		return m.setAsideS3(ctx, rec)
	default:
		return Repository{}, fmt.Errorf("unknown backend %q", rec.Backend)
	}
}

// setAsideLocal renames a local repository's directory. A rename is
// atomic, so a crash leaves either the repository in place, to be set
// aside on the next try, or no repository, to be created on the next try.
func (m *Manager) setAsideLocal(ctx context.Context, rec Repository) (Repository, error) {
	dir, err := m.localDir(rec)
	if err != nil {
		return Repository{}, err
	}
	base := filepath.Join(filepath.Dir(m.layout.BackupRepositoriesDir()), unopenablePrefix+rec.ClientGroupID+"-"+timestamp())
	dest, err := freeName(base, func(p string) (bool, error) {
		_, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return err == nil, err
	})
	if err != nil {
		return Repository{}, fmt.Errorf("setting aside the repository at %s: %w", dir, err)
	}
	if err := os.Rename(dir, dest); err != nil {
		return Repository{}, fmt.Errorf("setting aside the repository at %s: %w", dir, err)
	}
	m.logSetAside(rec, dest, rec.Location)
	// The files have moved either way; the log above has the location.
	if err := auditSetAside(ctx, m.db, rec, dest, rec.Location); err != nil {
		m.log.Error("recording a set-aside backup repository in the audit log failed",
			"client_group_id", rec.ClientGroupID, "unopenable_location", dest, "error", err)
	}
	return rec, nil
}

// setAsideS3 leaves an S3 repository's objects where they are and points
// the record at a new location next to them, <group>-<time> under the same
// prefix, where the new repository is created. Copying every object to
// another prefix and deleting the originals would be slow for a large
// repository, could stop halfway, and would delete objects; moving the
// record is one transaction. The old objects are left for the operator.
func (m *Manager) setAsideS3(ctx context.Context, rec Repository) (Repository, error) {
	obj, err := parseS3Location(rec.Location)
	if err != nil {
		return Repository{}, err
	}
	if !strings.HasSuffix(rec.Location, "/"+obj.key) {
		return Repository{}, fmt.Errorf("can't set aside the repository at %s: unexpected location", rec.Location)
	}
	c, err := m.s3Client(obj)
	if err != nil {
		return Repository{}, err
	}
	key, err := freeName(path.Join(path.Dir(obj.key), rec.ClientGroupID+"-"+timestamp()), func(k string) (bool, error) {
		keys, err := c.list(ctx, k+"/")
		return len(keys) > 0, err
	})
	if err != nil {
		return Repository{}, fmt.Errorf("setting aside the repository at %s: %w", rec.Location, err)
	}
	location := strings.TrimSuffix(rec.Location, obj.key) + key
	moved := false
	err = m.db.InTx(ctx, func(q store.Querier) error {
		var err error
		if moved, err = moveRecord(ctx, q, rec, location); err != nil || !moved {
			return err
		}
		if err := store.NewClientGroups(q).SetBackupRepository(ctx, rec.ClientGroupID, location); err != nil {
			return err
		}
		return auditSetAside(ctx, q, rec, rec.Location, location)
	})
	if err != nil {
		return Repository{}, fmt.Errorf("setting aside the repository at %s: %w", rec.Location, err)
	}
	if !moved {
		return getRecord(ctx, m.db, rec.ClientGroupID)
	}
	m.logSetAside(rec, rec.Location, location)
	rec.Location = location
	return rec, nil
}

// logSetAside says loudly where a set-aside repository went: its files
// can't be opened, but they may hold the group's only copy of some
// backups, so the operator decides what becomes of them.
func (m *Manager) logSetAside(rec Repository, unopenable, location string) {
	m.log.Warn("backup repository set aside: its stored password doesn't open it",
		"client_group_id", rec.ClientGroupID, "backend", rec.Backend,
		"unopenable_location", unopenable, "new_location", location)
}

// auditSetAside records in the audit log where a set-aside repository
// went, so that it can be found after the control plane's log is gone.
func auditSetAside(ctx context.Context, q store.Querier, rec Repository, unopenable, location string) error {
	return store.NewAuditEvents(q).Insert(ctx, &store.AuditEvent{
		Action:     AuditActionSetAside,
		TargetType: auditTargetClientGroup,
		TargetID:   rec.ClientGroupID,
		Result:     store.AuditSuccess,
		Details: map[string]string{
			"actor":               "control-plane",
			"backend":             rec.Backend,
			"unopenable_location": unopenable,
			"new_location":        location,
		},
	})
}

// auditTargetClientGroup is authz.TargetClientGroup, the target type of
// audit events about a client group.
const auditTargetClientGroup = "client_group"

// timestamp names a set-aside repository by when it was set aside.
func timestamp() string { return time.Now().UTC().Format("20060102T150405Z") }

// freeName returns base, or base-2, base-3, ... if it is taken, so a
// repository set aside twice within a second doesn't land on the first.
func freeName(base string, taken func(string) (bool, error)) (string, error) {
	const maxTries = 100
	for i := 1; i <= maxTries; i++ {
		name := base
		if i > 1 {
			name += "-" + strconv.Itoa(i)
		}
		t, err := taken(name)
		if err != nil {
			return "", err
		}
		if !t {
			return name, nil
		}
	}
	return "", fmt.Errorf("%s and %d names after it are taken", base, maxTries-1)
}
