package repository

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// ErrRepositoryDamaged means a repository has several key files and restic
// can't verify its config with any of them: concurrent `restic init` runs
// each wrote a master key (#188), and the config is encrypted with only
// one. The repository is left alone; the error says how to repair it.
var ErrRepositoryDamaged = errors.New("repository: repository has conflicting key files")

// ErrRepositoryUnfinished means a repository has no config but does have
// snapshots or an index, so it isn't the leftover of an interrupted init
// and is not touched.
var ErrRepositoryUnfinished = errors.New("repository: repository has no config but has data")

// resticCorruptKey is what restic says when the key it opened doesn't
// decrypt the config.
const resticCorruptKey = "ciphertext verification failed"

// entries returns the names in one of a repository's subdirectories (keys,
// snapshots, index), or none if it doesn't exist.
func (m *Manager) entries(ctx context.Context, rec Repository, sub string) ([]string, error) {
	switch rec.Backend {
	case BackendLocal:
		dir, err := m.localDir(rec)
		if err != nil {
			return nil, err
		}
		des, err := os.ReadDir(filepath.Join(dir, sub))
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("listing repository %s: %w", sub, err)
		}
		var names []string
		for _, de := range des {
			if !de.IsDir() {
				names = append(names, de.Name())
			}
		}
		return names, nil
	case BackendS3:
		obj, err := parseS3Location(rec.Location)
		if err != nil {
			return nil, err
		}
		c, err := m.s3Client(obj)
		if err != nil {
			return nil, err
		}
		keys, err := c.list(ctx, obj.key+"/"+sub+"/")
		if err != nil {
			return nil, err
		}
		var names []string
		for _, k := range keys {
			if name := path.Base(k); !strings.HasSuffix(k, "/") && name != "" {
				names = append(names, name)
			}
		}
		return names, nil
	default:
		return nil, fmt.Errorf("unknown backend %q", rec.Backend)
	}
}

// removeEntries deletes the named files of one of a repository's
// subdirectories.
func (m *Manager) removeEntries(ctx context.Context, rec Repository, sub string, names []string) error {
	switch rec.Backend {
	case BackendLocal:
		dir, err := m.localDir(rec)
		if err != nil {
			return err
		}
		for _, n := range names {
			if err := os.Remove(filepath.Join(dir, sub, n)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("removing repository %s file: %w", sub, err)
			}
		}
		return nil
	case BackendS3:
		obj, err := parseS3Location(rec.Location)
		if err != nil {
			return err
		}
		c, err := m.s3Client(obj)
		if err != nil {
			return err
		}
		for _, n := range names {
			if err := c.remove(ctx, obj.key+"/"+sub+"/"+n); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unknown backend %q", rec.Backend)
	}
}

// clearInterruptedInit removes the key files an interrupted `restic init`
// left in a repository that has no config. restic writes its master key
// before the config, so a control plane that stopped in between leaves a
// key for which no config will ever exist; a later init would add a second
// master key and break the repository the same way racing inits do. Without
// a config nothing can have been backed up, so no data depends on the key.
// It refuses, with ErrRepositoryUnfinished, if there are snapshots or an
// index.
func (m *Manager) clearInterruptedInit(ctx context.Context, rec Repository) error {
	keys, err := m.entries(ctx, rec, "keys")
	if err != nil || len(keys) == 0 {
		return err
	}
	for _, sub := range []string{"snapshots", "index"} {
		names, err := m.entries(ctx, rec, sub)
		if err != nil {
			return err
		}
		if len(names) > 0 {
			return fmt.Errorf("%w: %s holds %d files and keys holds %d; "+
				"it is not an interrupted init, so it is left alone (location %s)", ErrRepositoryUnfinished, sub, len(names), len(keys), rec.Location)
		}
	}
	if err := m.removeEntries(ctx, rec, "keys", keys); err != nil {
		return err
	}
	m.log.Warn("removed key files left by an interrupted restic init",
		"client_group_id", rec.ClientGroupID, "location", rec.Location, "keys", len(keys))
	return nil
}

// diagnose turns restic's "ciphertext verification failed" on a repository
// with several key files into ErrRepositoryDamaged, naming the key files
// and the repair. Any other error comes back unchanged. It never deletes
// anything: which key file is the right one is the operator's call.
func (m *Manager) diagnose(ctx context.Context, rec Repository, err error) error {
	if !strings.Contains(err.Error(), resticCorruptKey) {
		return err
	}
	keys, kerr := m.entries(ctx, rec, "keys")
	if kerr != nil || len(keys) < 2 {
		return err
	}
	slices.Sort(keys)
	where := rec.Location + "/keys/"
	return fmt.Errorf("%w: %s holds %d key files (%s) and restic can't verify the config with the one it opens "+
		"(%s). Concurrent `restic init` runs wrote a master key each, and the config is encrypted with only one of them (issue #188). "+
		"The backups are intact but unreadable until the extra key files are removed. To repair it, move all key files but one out of %s, "+
		"and run `restic -r %s cat config` with the group's repository password (`pressfleet repository export <client-group-id> <directory>` writes it to password.txt); "+
		"repeat with each key file until the command succeeds, then delete the others. If it holds no backups yet, "+
		"deleting the repository's files (all of %s) is enough: the next backup creates it again",
		ErrRepositoryDamaged, where, len(keys), strings.Join(keys, ", "), resticCorruptKey, where, rec.Location, rec.Location)
}
