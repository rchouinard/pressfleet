package repository

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
)

// The state repository holds the disaster recovery backups of the state
// database (#96, package backup/statebackup). It sits beside the client
// group repositories, on the same backend: <data dir>/backups/state
// (config.Layout.StateBackupRepositoryDir) or <prefix>/state in the S3
// bucket, a name no client group ID (a ULID) can take. Its password is
// derived from the master key (secrets.Keyring.StateBackupPassword), since
// it can't be stored in the database it backs up: a new host has only the
// master key, its configuration, and the backups.
//
// Its restic commands run like the group-level ones, as child processes of
// the control plane with the same environment rules (restic.go), so they
// move with them should group-level restic move into task containers
// (#178).

// stateRepositoryName is the state repository's name on S3.
const stateRepositoryName = "state"

// ErrNoStatePassword means the Manager was built without a state
// repository password.
var ErrNoStatePassword = errors.New("repository: the state repository password is not configured")

// StateLocation returns where the state repository is: a directory or an
// s3: URL, without credentials.
func (m *Manager) StateLocation() string {
	return m.stateRecord().Location
}

// stateRecord returns the state repository as a Repository.
func (m *Manager) stateRecord() Repository {
	rec := Repository{Backend: m.backend, Location: m.layout.StateBackupRepositoryDir()}
	if m.backend == BackendS3 {
		rec.Location = s3Location(m.s3, stateRepositoryName)
	}
	return rec
}

// EnsureState creates the state repository unless it exists and opens
// with the password. It is idempotent.
func (m *Manager) EnsureState(ctx context.Context) error {
	if m.statePassword == "" {
		return ErrNoStatePassword
	}
	rec := m.stateRecord()
	err := m.probe(ctx, rec, m.statePassword)
	if !errors.Is(err, errRepositoryMissing) {
		if err != nil {
			return fmt.Errorf("opening the state repository: %w", err)
		}
		return nil
	}
	if rec.Backend == BackendLocal {
		// Only the control plane uses it: no group sharing, unlike the
		// client group repositories that site tasks open.
		if err := os.MkdirAll(rec.Location, 0o700); err != nil {
			return fmt.Errorf("creating the state repository directory: %w", err)
		}
	}
	if _, err := m.run(ctx, rec, m.statePassword, "init"); err != nil {
		if m.probe(ctx, rec, m.statePassword) != nil {
			return fmt.Errorf("creating the state repository: %w", err)
		}
		return nil
	}
	m.log.Info("state backup repository initialized", "location", rec.Location)
	return nil
}

// StateRestic runs a restic command against the state repository,
// connecting stdin (nil for none) and stdout. It returns ErrNoRepository
// if the repository doesn't exist yet.
func (m *Manager) StateRestic(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error {
	if m.statePassword == "" {
		return ErrNoStatePassword
	}
	rec := m.stateRecord()
	if rec.Backend == BackendS3 {
		// See probe: restic would retry a missing bucket for minutes.
		if err := m.probe(ctx, rec, m.statePassword); errors.Is(err, errRepositoryMissing) {
			return fmt.Errorf("state repository at %s: %w", rec.Location, ErrNoRepository)
		}
	}
	err := m.runIO(ctx, rec, m.statePassword, stdin, stdout, args...)
	if errors.Is(err, errRepositoryMissing) {
		return fmt.Errorf("state repository at %s: %w", rec.Location, ErrNoRepository)
	}
	return err
}
