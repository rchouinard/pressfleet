package metrics_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/metrics"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// addBackup inserts a finished backup record with exact times.
func addBackup(t *testing.T, db *store.DB, siteID, groupID string, status store.BackupStatus, started, finished time.Time, size int64) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		"INSERT INTO backup_records (id, site_id, client_group_id, kind, status, wp_core_version, size_bytes, created_at, started_at, finished_at) "+
			"VALUES (?, ?, ?, 'scheduled', ?, '7.1.2', ?, ?, ?, ?)",
		id.New(), siteID, groupID, string(status), size, started, started, finished); err != nil {
		t.Fatal(err)
	}
}

func addJob(t *testing.T, db *store.DB, state store.JobState) {
	t.Helper()
	j := store.Job{Type: "test.job", TargetType: store.JobTargetSite, TargetID: id.New(), Payload: json.RawMessage(`{}`)}
	if err := store.NewJobs(db).Enqueue(context.Background(), &j); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), "UPDATE jobs SET state = ? WHERE id = ?", string(state), j.ID); err != nil {
		t.Fatal(err)
	}
}

// find returns the value of the sample with the name and the label pairs.
func findSample(samples []metrics.Sample, name string, labels ...string) (float64, bool) {
next:
	for _, s := range samples {
		if s.Name != name {
			continue
		}
		for i := 0; i < len(labels); i += 2 {
			if s.Labels[labels[i]] != labels[i+1] {
				continue next
			}
		}
		return s.Value, true
	}
	return 0, false
}

func TestBackupJobsCollector(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		base := time.Now().Add(-48 * time.Hour).Truncate(time.Second).UTC()

		okSite, okGroup := addSite(t, db, store.SiteRunning)
		failSite, failGroup := addSite(t, db, store.SiteRunning)
		offSite, offGroup := addSite(t, db, store.SiteRunning)
		neverSite, _ := addSite(t, db, store.SiteRunning)

		// okSite: an old failure, then a success that is the latest.
		addBackup(t, db, okSite, okGroup, store.BackupFailed, base, base.Add(time.Minute), 0)
		addBackup(t, db, okSite, okGroup, store.BackupSucceeded, base.Add(time.Hour), base.Add(time.Hour+90*time.Second), 1<<30)
		// failSite: a success, then a failure that is the latest.
		addBackup(t, db, failSite, failGroup, store.BackupSucceeded, base, base.Add(30*time.Second), 500)
		addBackup(t, db, failSite, failGroup, store.BackupFailed, base.Add(time.Hour), base.Add(2*time.Hour), 0)
		// offSite has records but its schedule is off.
		addBackup(t, db, offSite, offGroup, store.BackupSucceeded, base, base.Add(time.Minute), 1)
		if _, err := db.ExecContext(ctx, "UPDATE sites SET backup_schedule_enabled = ? WHERE id = ?", false, offSite); err != nil {
			t.Fatal(err)
		}
		// A running backup is neither a success nor a failure.
		if _, err := db.ExecContext(ctx,
			"INSERT INTO backup_records (id, site_id, client_group_id, kind, status, wp_core_version, created_at, started_at) VALUES (?, ?, ?, 'manual', 'running', '7.1.2', ?, ?)",
			id.New(), okSite, okGroup, time.Now(), time.Now()); err != nil {
			t.Fatal(err)
		}

		for range 2 {
			addJob(t, db, store.JobQueued)
		}
		addJob(t, db, store.JobRunning)
		addJob(t, db, store.JobRollingBack)
		for range 3 {
			addJob(t, db, store.JobSucceeded)
		}
		addJob(t, db, store.JobFailed)
		addJob(t, db, store.JobRollbackFailed)

		targets, err := metrics.NewSiteTargets(db).Targets(ctx)
		if err != nil {
			t.Fatal(err)
		}
		c := metrics.BackupJobsCollector{DB: db}
		samples, err := c.Collect(ctx, targets)
		if err != nil {
			t.Fatal(err)
		}

		for _, tc := range []struct {
			name   string
			labels []string
			want   float64
		}{
			{"pressfleet_jobs_queue_depth", []string{"state", "queued"}, 2},
			{"pressfleet_jobs_queue_depth", []string{"state", "running"}, 2},
			{"pressfleet_jobs_finished_total", []string{"result", "succeeded"}, 3},
			{"pressfleet_jobs_finished_total", []string{"result", "failed"}, 2},
			{"pressfleet_site_backup_last_run_success", []string{"site_id", okSite}, 1},
			{"pressfleet_site_backup_last_success_timestamp_seconds", []string{"site_id", okSite}, float64(base.Add(time.Hour + 90*time.Second).Unix())},
			{"pressfleet_site_backup_size_bytes", []string{"site_id", okSite}, 1 << 30},
			{"pressfleet_site_backup_duration_seconds", []string{"site_id", okSite}, 90},
			{"pressfleet_site_backup_last_run_success", []string{"site_id", failSite, "client_group_id", failGroup}, 0},
			{"pressfleet_site_backup_last_success_timestamp_seconds", []string{"site_id", failSite}, float64(base.Add(30 * time.Second).Unix())},
			{"pressfleet_site_backup_size_bytes", []string{"site_id", failSite}, 500},
			{"pressfleet_site_backup_duration_seconds", []string{"site_id", failSite}, 30},
		} {
			got, ok := findSample(samples, tc.name, tc.labels...)
			if !ok || got != tc.want {
				t.Errorf("%s%v = %v (found %v), want %v", tc.name, tc.labels, got, ok, tc.want)
			}
		}
		for _, siteID := range []string{offSite, neverSite} {
			for _, name := range []string{"pressfleet_site_backup_last_run_success", "pressfleet_site_backup_last_success_timestamp_seconds"} {
				if _, ok := findSample(samples, name, "site_id", siteID); ok {
					t.Errorf("%s emitted for site %s, which has no schedule or no backups", name, siteID)
				}
			}
		}
	})
}

// A staging site has no backup schedule by default, so it emits no backup
// health series, and the built-in backup alerts never see it. One that
// turns its schedule on is reported like any other site.
func TestBackupJobsCollectorSkipsStagingSites(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		base := time.Now().Add(-time.Hour).Truncate(time.Second).UTC()
		prod, _ := addSite(t, db, store.SiteRunning)
		staging, stagingGroup := addSite(t, db, store.SiteRunning)
		scheduled, scheduledGroup := addSite(t, db, store.SiteRunning)
		makeStaging(t, db, staging, prod)
		makeStaging(t, db, scheduled, prod)
		addBackup(t, db, staging, stagingGroup, store.BackupFailed, base, base.Add(time.Minute), 0)
		addBackup(t, db, scheduled, scheduledGroup, store.BackupSucceeded, base, base.Add(time.Minute), 1)
		for _, siteID := range []string{staging, scheduled} {
			enabled := siteID == scheduled
			if _, err := db.ExecContext(ctx, "UPDATE sites SET backup_schedule_enabled = ? WHERE id = ?", enabled, siteID); err != nil {
				t.Fatal(err)
			}
		}

		targets, err := metrics.NewSiteTargets(db).Targets(ctx)
		if err != nil {
			t.Fatal(err)
		}
		samples, err := metrics.BackupJobsCollector{DB: db}.Collect(ctx, targets)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range samples {
			if s.Labels["site_id"] == staging {
				t.Errorf("sample %s emitted for a staging site with its backup schedule off", s.Name)
			}
		}
		if v, ok := findSample(samples, "pressfleet_site_backup_last_run_success", "site_id", scheduled); !ok || v != 1 {
			t.Errorf("last_run_success of a staging site with a schedule = %v (found %v), want 1", v, ok)
		}
	})
}

func TestBackupJobsCollectorMetadata(t *testing.T) {
	var c metrics.Collector = metrics.BackupJobsCollector{Every: time.Minute}
	if c.Name() != "backup_jobs" || c.Interval() != time.Minute {
		t.Errorf("name %q, interval %v", c.Name(), c.Interval())
	}
}
