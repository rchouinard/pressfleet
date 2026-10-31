package sites_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// The copy times of a staging site (#208) say how old its database and
// uploads are: the time of the snapshot each was copied from, not of the job.
// The tests give the site known times first, so a time a job leaves alone
// differs from one it sets, and use snapshots whose times are neither the
// job's (testNow) nor the current time.

var (
	// wasDB and wasUploads are what the staging site is given before a job.
	wasDB      = time.Date(2026, 10, 5, 8, 0, 0, 111111000, time.UTC)
	wasUploads = time.Date(2026, 10, 6, 8, 0, 0, 222222000, time.UTC)
	// olderSnap is the time of an older named backup.
	olderSnap = time.Date(2026, 10, 12, 7, 30, 15, 123456789, time.UTC)
)

// stagingAsOf are a staging site's two copy times; the zero time is NULL.
type stagingAsOf struct{ database, uploads time.Time }

func asOfOf(s store.Site) stagingAsOf {
	return stagingAsOf{s.StagingDatabaseAsOf, s.StagingUploadsAsOf}
}

// requireAsOf compares copy times as stored, which keeps microseconds.
func requireAsOf(t *testing.T, what string, got, want stagingAsOf) {
	t.Helper()
	trunc := func(v time.Time) time.Time { return v.UTC().Truncate(time.Microsecond) }
	if !got.database.Equal(trunc(want.database)) || !got.uploads.Equal(trunc(want.uploads)) {
		t.Errorf("%s: database as of %v, uploads as of %v; want %v and %v", what,
			got.database, got.uploads, want.database, want.uploads)
	}
}

// giveAsOf sets the staging site's copy times, as an earlier job would have.
func giveAsOf(t *testing.T, db *store.DB, siteID string, at stagingAsOf) {
	t.Helper()
	if _, err := store.NewSites(db).SetStagingAsOf(context.Background(), siteID,
		store.AsOfSet(at.database), store.AsOfSet(at.uploads)); err != nil {
		t.Fatal(err)
	}
}

// newestSnapshot returns the site's newest snapshot as the engine reports it.
func (e *restoreEnv) newestSnapshot(t *testing.T, siteID string) backup.Snapshot {
	t.Helper()
	snaps, err := e.engine.ListSnapshots(context.Background(), siteID)
	if err != nil || len(snaps) == 0 {
		t.Fatalf("snapshots of %s: %v, %v", siteID, snaps, err)
	}
	return snaps[len(snaps)-1]
}

func TestCloneRecordsSnapshotTime(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newRefreshEnv(t, db)
		snap := e.newestSnapshot(t, e.prod.ID)
		if string(snap.ID) != e.stg.StagingSnapshotID || snap.Time.IsZero() || snap.Time.Equal(testNow) {
			t.Fatalf("snapshot %v, staging site cloned from %q", snap, e.stg.StagingSnapshotID)
		}
		requireAsOf(t, "after the clone", asOfOf(e.stg), stagingAsOf{snap.Time, snap.Time})
		if !e.stg.StagingRefreshedAt.Equal(testNow) {
			t.Errorf("refreshed at %v, want the job's time %v", e.stg.StagingRefreshedAt, testNow)
		}
	})
}

func TestRefreshRecordsSnapshotTime(t *testing.T) {
	for _, tc := range []struct {
		name     string
		files    backup.RestoreFiles
		database bool
		// named copies an older named snapshot; otherwise a fresh backup.
		named bool
		// untimed makes the snapshot lack a time, like a job queued before
		// times were recorded.
		untimed bool
		want    func(snapAt time.Time) stagingAsOf
	}{
		{"database, older backup", backup.RestoreFilesNone, true, true, false,
			func(at time.Time) stagingAsOf { return stagingAsOf{at, wasUploads} }},
		{"database with code, older backup", backup.RestoreFilesCode, true, true, false,
			func(at time.Time) stagingAsOf { return stagingAsOf{at, wasUploads} }},
		{"all files, older backup", backup.RestoreFilesAll, false, true, false,
			func(at time.Time) stagingAsOf { return stagingAsOf{wasDB, at} }},
		{"everything, older backup", backup.RestoreFilesAll, true, true, false,
			func(at time.Time) stagingAsOf { return stagingAsOf{at, at} }},
		{"code only, older backup", backup.RestoreFilesCode, false, true, false,
			func(time.Time) stagingAsOf { return stagingAsOf{wasDB, wasUploads} }},
		{"everything, fresh backup", backup.RestoreFilesAll, true, false, false,
			func(at time.Time) stagingAsOf { return stagingAsOf{at, at} }},
		{"code only, fresh backup", backup.RestoreFilesCode, false, false, false,
			func(time.Time) stagingAsOf { return stagingAsOf{wasDB, wasUploads} }},
		{"everything, snapshot without a time", backup.RestoreFilesAll, true, true, true,
			func(time.Time) stagingAsOf { return stagingAsOf{} }},
		{"database, snapshot without a time", backup.RestoreFilesNone, true, true, true,
			func(time.Time) stagingAsOf { return stagingAsOf{time.Time{}, wasUploads} }},
		{"code only, snapshot without a time", backup.RestoreFilesCode, false, true, true,
			func(time.Time) stagingAsOf { return stagingAsOf{wasDB, wasUploads} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storetest.Run(t, func(t *testing.T, db *store.DB) {
				e := newRefreshEnv(t, db)
				giveAsOf(t, db, e.stg.ID, stagingAsOf{wasDB, wasUploads})
				req := sites.StagingRefresh{Files: tc.files, Database: tc.database}
				snapAt := olderSnap
				if tc.named {
					snap := e.snapshot(t, e.prod.ID)
					e.engine.setTime(snap.ID, olderSnap)
					if tc.untimed {
						snapAt = time.Time{}
						e.engine.setTime(snap.ID, snapAt)
					}
					req.SnapshotID = string(snap.ID)
				}
				_, d := e.refresh(t, req)
				if d.Job.State != store.JobSucceeded {
					t.Fatalf("refresh job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
				}
				if !tc.named {
					snapAt = e.newestSnapshot(t, e.prod.ID).Time
				}
				got := e.site(t, e.stg.ID)
				requireAsOf(t, "after the refresh", asOfOf(got), tc.want(snapAt))
				// The job's own time is kept apart.
				if !got.StagingRefreshedAt.Equal(testNow) {
					t.Errorf("refreshed at %v, want the job's time %v", got.StagingRefreshedAt, testNow)
				}
			})
		})
	}
}

// A refresh that fails and rolls back records nothing: the times describe the
// data the rollback puts back.
func TestRefreshFailureKeepsSnapshotTimes(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newRefreshEnv(t, db)
		giveAsOf(t, db, e.stg.ID, stagingAsOf{wasDB, wasUploads})
		fatal := &provisioner.ExecError{ExitCode: 255, Stderr: "PHP Fatal error: boom"}
		e.setFailWP(func(_ string, args []string) error {
			if args[0] == "search-replace" {
				return fatal
			}
			return nil
		})
		_, d := e.refresh(t, sites.StagingRefresh{Files: backup.RestoreFilesAll, Database: true})
		if d.Job.State != store.JobFailed {
			t.Fatalf("refresh job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
		}
		if got := e.site(t, e.stg.ID); !strings.Contains(got.StatusMessage, "rolled back") {
			t.Fatalf("the refresh didn't roll back: %q", got.StatusMessage)
		} else {
			requireAsOf(t, "after the rollback", asOfOf(got), stagingAsOf{wasDB, wasUploads})
		}
	})
}

func TestPushRecordsSnapshotTime(t *testing.T) {
	for _, tc := range []struct {
		name     string
		files    backup.RestoreFiles
		database bool
		want     func(snapAt time.Time) stagingAsOf
	}{
		{"database", backup.RestoreFilesNone, true, func(at time.Time) stagingAsOf { return stagingAsOf{at, wasUploads} }},
		{"database with code", backup.RestoreFilesCode, true, func(at time.Time) stagingAsOf { return stagingAsOf{at, wasUploads} }},
		{"all files", backup.RestoreFilesAll, false, func(at time.Time) stagingAsOf { return stagingAsOf{wasDB, at} }},
		{"everything", backup.RestoreFilesAll, true, func(at time.Time) stagingAsOf { return stagingAsOf{at, at} }},
		{"code only", backup.RestoreFilesCode, false, func(time.Time) stagingAsOf { return stagingAsOf{wasDB, wasUploads} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storetest.Run(t, func(t *testing.T, db *store.DB) {
				e := newPushEnv(t, db)
				giveAsOf(t, db, e.stg.ID, stagingAsOf{wasDB, wasUploads})
				_, d := e.push(t, sites.StagingPush{Files: tc.files, Database: tc.database})
				if d.Job.State != store.JobSucceeded {
					t.Fatalf("push job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
				}
				snap := e.newestSnapshot(t, e.stg.ID)
				if snap.Time.IsZero() || snap.Time.Equal(testNow) {
					t.Fatalf("staging snapshot %+v doesn't tell it from the job's time", snap)
				}
				got := e.site(t, e.stg.ID)
				requireAsOf(t, "after the push", asOfOf(got), tc.want(snap.Time))
				if !got.StagingPushedAt.Equal(testNow) {
					t.Errorf("pushed at %v, want the job's time %v", got.StagingPushedAt, testNow)
				}
			})
		})
	}
}

func TestPushFailureKeepsSnapshotTimes(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newPushEnv(t, db)
		giveAsOf(t, db, e.stg.ID, stagingAsOf{wasDB, wasUploads})
		fatal := &provisioner.ExecError{ExitCode: 255, Stderr: "PHP Fatal error: boom"}
		e.setFailWP(func(siteID string, args []string) error {
			if siteID == e.prod.ID && args[0] == "search-replace" {
				return fatal
			}
			return nil
		})
		_, d := e.push(t, sites.StagingPush{Files: backup.RestoreFilesAll, Database: true})
		if d.Job.State != store.JobFailed {
			t.Fatalf("push job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
		}
		if got := e.site(t, e.prod.ID); !strings.Contains(got.StatusMessage, "rolled back") {
			t.Fatalf("the push didn't roll back: %q", got.StatusMessage)
		}
		got := e.site(t, e.stg.ID)
		requireAsOf(t, "after the failed push", asOfOf(got), stagingAsOf{wasDB, wasUploads})
		if !got.StagingPushedAt.IsZero() {
			t.Errorf("a failed push was recorded: %v", got.StagingPushedAt)
		}
	})
}

// A restore of a staging site puts back data of an unknown age, whichever
// site the snapshot is from, so the times are cleared. A restore of a
// production site has no copy times to change.
func TestRestoreClearsSnapshotTimes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fromProd bool
	}{{"its own backup", false}, {"production's backup", true}} {
		t.Run(tc.name, func(t *testing.T) {
			storetest.Run(t, func(t *testing.T, db *store.DB) {
				e := newRefreshEnv(t, db)
				giveAsOf(t, db, e.stg.ID, stagingAsOf{wasDB, wasUploads})
				req := sites.Restore{}
				if tc.fromProd {
					req.SourceSiteID, req.SnapshotID = e.prod.ID, string(e.snapshot(t, e.prod.ID).ID)
				} else {
					req.SnapshotID = string(e.snapshot(t, e.stg.ID).ID)
				}
				if d := e.restore(t, e.stg.ID, req); d.Job.State != store.JobSucceeded {
					t.Fatalf("restore job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
				}
				requireAsOf(t, "after the restore", asOfOf(e.site(t, e.stg.ID)), stagingAsOf{})
			})
		})
	}
}

func TestRecoverSetsSnapshotTimes(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newRestoreEnv(t, db)
		prod := e.runningSite(t, "Prod", "post=old")
		prodSnap := e.snapshot(t, prod.ID)
		e.engine.setTime(prodSnap.ID, olderSnap)

		fromProd := e.stagingOf(t, e.runningSite(t, "FromProd", "post=a"), prod, true)
		own := e.stagingOf(t, e.runningSite(t, "Own", "post=b"), prod, true)
		e.snapshot(t, own.ID)
		// A job queued before times were recorded: the same as a snapshot
		// that doesn't say when it was taken.
		prod2 := e.runningSite(t, "Prod2", "post=c")
		untimedSnap := e.snapshot(t, prod2.ID)
		e.engine.setTime(untimedSnap.ID, time.Time{})
		untimed := e.stagingOf(t, e.runningSite(t, "Untimed", "post=d"), prod2, true)

		for _, s := range []store.Site{fromProd, own, untimed} {
			giveAsOf(t, db, s.ID, stagingAsOf{wasDB, wasUploads})
		}
		all := []store.Site{prod, fromProd, own, prod2, untimed}
		var ids []string
		for _, s := range all {
			ids = append(ids, s.ID)
		}
		e.loseHost(t, ids...)
		if rec := e.prepareRecovery(t); len(rec.Recovering) != len(all) {
			t.Fatalf("recovering %v, want %d sites", rec.Recovering, len(all))
		}
		e.runJobs(t)

		for _, tc := range []struct {
			name string
			site store.Site
			want stagingAsOf
		}{
			{"from production's snapshot", fromProd, stagingAsOf{olderSnap, olderSnap}},
			{"from its own snapshot", own, stagingAsOf{}},
			{"from production's snapshot without a time", untimed, stagingAsOf{}},
		} {
			if d := e.recoverJob(t, tc.site.ID); d.Job.State != store.JobSucceeded {
				t.Fatalf("%s: recover job %s %s\n%s", tc.name, d.Job.State, d.Job.Error, jobLog(d))
			}
			requireAsOf(t, tc.name, asOfOf(e.site(t, tc.site.ID)), tc.want)
		}
	})
}
