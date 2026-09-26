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
