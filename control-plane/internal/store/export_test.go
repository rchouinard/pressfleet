package store

import "context"

// MigrateTo applies the first n migrations, for tests of data migrations.
func (db *DB) MigrateTo(ctx context.Context, backupDir string, n int) (MigrateResult, error) {
	ms, err := Migrations(db.dialect)
	if err != nil {
		return MigrateResult{}, err
	}
	return db.migrate(ctx, backupDir, ms[:n])
}

// MigrateWithExtra applies every migration and then extra ones, given as
// SQL, for tests of runs that fail partway.
func (db *DB) MigrateWithExtra(ctx context.Context, backupDir string, extra ...string) (MigrateResult, error) {
	ms, err := Migrations(db.dialect)
	if err != nil {
		return MigrateResult{}, err
	}
	for _, q := range extra {
		ms = append(ms, Migration{Version: len(ms) + 1, Name: "test_extra", sql: q})
	}
	return db.migrate(ctx, backupDir, ms)
}
