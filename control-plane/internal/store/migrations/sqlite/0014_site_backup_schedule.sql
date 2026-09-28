-- Scheduled backups (#53, design doc §5.3). Keep in sync with
-- migrations/postgres/0014_site_backup_schedule.sql.
--
-- Each site's backup schedule, which package backup/sitebackup turns into a
-- row in schedules named backup:<site ID>. Every site is backed up daily by
-- default, at a time spread over the platform's backup window
-- (backup.schedule in the config file). backup_interval_hours is the time
-- between backups: a divisor of 24, or a multiple of 24 up to 168.
-- backup_hour, when set, is the UTC hour the site's backups start in instead
-- of the platform's window. With backup_schedule_enabled false the site has
-- no schedule; manual backups still work.
ALTER TABLE sites ADD COLUMN backup_schedule_enabled BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE sites ADD COLUMN backup_interval_hours INTEGER NOT NULL DEFAULT 24;
ALTER TABLE sites ADD COLUMN backup_hour INTEGER;
